package dumate

// 本文件把百度搭子适配成 core.Vendor。
//
// 与其它供应商的关键差异：
//   - **凭证是 Cookie，不是 OAuth 令牌**：长期凭据只有一份网页登录态，
//     短期票据（gateway token，约 1 小时）由插件自己换、自己续；
//   - **登录是百度账号扫码**：面板发起后展示二维码，用户用百度 App 扫码确认，
//     插件拿到 BDUSS/STOKEN 后直接落盘（见 login.go）。也支持直接粘 Cookie；
//   - **协议就是 OpenAI**：云端网关原生 chat-completions，不需要协议翻译；
//   - **额度是积分制**：签到领积分，对话扣积分，余额查询走网页接口。

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// cachedToken 是内存里的网关票据缓存。
//
// 为什么必须缓存：宿主不会把 token 写回凭证文件（文件里只有 Cookie），
// 于是每次解析出来的 Credential.Token 都是空，RefreshNative 每次都会被
// 判成「没票」而重新兑一次。实测表现为发一条消息就刷一行「已换取网关票据」，
// 十几条请求就把账号打到 429。票据本身 22 小时有效，缓存在进程里最省事。
type cachedToken struct {
	token   string
	expires int64
}

// adapter 是 core.Vendor 在搭子上的实现。
type adapter struct {
	*core.NativeAdapter[*Credential]
	webBase     string
	gatewayHost string
	// retryOn401 报告是否在 token 被提前作废时强制刷新重试一次。
	retryOn401 bool
	// tokenMu 保护 tokens；tokens 以 Cookie 指纹为键，Cookie 一换自然重换票。
	tokenMu sync.Mutex
	tokens  map[string]cachedToken
}

// tokenCacheKey 用 Cookie 的 SHA-1 做键：Cookie 很长，直接当 map 键浪费内存。
func tokenCacheKey(cookie string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(cookie)))
	return hex.EncodeToString(sum[:16])
}

// cachedTokenFor 返回仍未过期的缓存票据（没有则返回零值）。
func (v *adapter) cachedTokenFor(cookie string) cachedToken {
	v.tokenMu.Lock()
	defer v.tokenMu.Unlock()
	entry, ok := v.tokens[tokenCacheKey(cookie)]
	if !ok || entry.token == "" || entry.expires <= time.Now().Add(refreshSkew).Unix() {
		return cachedToken{}
	}
	return entry
}

// storeToken 记住刚换到的票据。
func (v *adapter) storeToken(cookie string, entry cachedToken) {
	v.tokenMu.Lock()
	defer v.tokenMu.Unlock()
	if v.tokens == nil {
		v.tokens = make(map[string]cachedToken)
	}
	v.tokens[tokenCacheKey(cookie)] = entry
}

// Instances 构造搭子实例（只有一个，没有区域之分）。
func Instances(env core.Env) []core.Vendor {
	inst := &adapter{retryOn401: true, tokens: make(map[string]cachedToken)}
	inst.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorID, VendorName: VendorName},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := ParseCredential(raw)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			label := core.FirstNonEmpty(native.Label, native.Nickname, native.UID, fileName)
			return core.CredentialSeed{
				VendorID:  VendorID,
				Label:     label,
				FileID:    core.FileIDFromFileName(fileName),
				UID:       native.UID,
				Token:     native.Token,
				ExpiresAt: native.ExpiresAt,
				AuthMode:  "cookie",
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			// 票据还在（内存缓存未过期）就什么都不做：token/get 也被限流，
			// 每个请求换一次会把账号打成 429。
			//
			// 判据必须和 ensureToken 用同一个余量（refreshSkew）：这里曾写成
			// 「ExpiresAt > 0 就算有效」，于是票据真正过期后宿主每 30 分钟的
			// 定时刷新也认为「不用换」，只有用户恰好发一次对话才被动续期——
			// 面板上就表现为「过期时间早就过了、也没人续」。
			cookie := native.CookieValue()
			if hit := inst.cachedTokenFor(cookie); hit.token != "" {
				native.Token = hit.token
				native.ExpiresAt = hit.expires
				return core.NativeRefresh[*Credential]{
					Native: native, Token: hit.token, ExpiresAt: hit.expires,
				}, nil
			}
			token, expires, errFetch := FetchToken(ctx, inst.webBase, cookie)
			if errFetch != nil {
				// 换不到新票但旧票还在：先让它顶着，别把整段时间变成 502。
				if native.BearerToken() != "" {
					return core.NativeRefresh[*Credential]{
						Native: native, Token: native.Token, ExpiresAt: native.ExpiresAt,
					}, errFetch
				}
				return core.NativeRefresh[*Credential]{}, errFetch
			}
			inst.storeToken(cookie, cachedToken{token: token, expires: expires})
			native.Token = token
			native.ExpiresAt = expires
			return core.NativeRefresh[*Credential]{
				Native: native, Token: token, ExpiresAt: expires, Changed: true,
			}, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeStorageJSON(original, native)
		},
	})
	return []core.Vendor{inst}
}

// Configure 解析供应商专属配置。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	v.webBase = WebBase(raw["dumate_web_base"])
	v.gatewayHost = GatewayHost(raw["dumate_gateway_host"])
}

// Match 判断一份凭证是否属于搭子。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// AuthMode 报告凭证形态（cookie）：页面据此提示「Cookie 登录」而不是 OAuth。
func (v *adapter) AuthMode() string { return "cookie" }

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := v.callWithTokenRetry(ctx, cred, native, req.Payload, false)
	if errChat != nil {
		return nil, errChat
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	if errToken := v.ensureToken(ctx, native); errToken != nil {
		return errToken
	}
	resp, errStream := ChatStream(ctx, v.gatewayHost, native.BearerToken(), req.Payload)
	if errStream != nil {
		// 401 说明票据被服务端提前作废（同账号在别处重新登录会顶掉旧票）。
		// 强刷一次再试——只重试一次，Cookie 真过期时重试无意义。
		if v.retryOn401 && isUnauthorized(errStream) {
			if errRefresh := v.forceRefreshToken(ctx, cred, native); errRefresh == nil {
				resp, errStream = ChatStream(ctx, v.gatewayHost, native.BearerToken(), req.Payload)
			}
		}
		if errStream != nil {
			return core.ToPluginError(errStream)
		}
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// callWithTokenRetry 执行一次非流式调用，带 401 强刷重试。
func (v *adapter) callWithTokenRetry(ctx context.Context, cred *core.Credential, native *Credential, body []byte, _ bool) ([]byte, error) {
	if errToken := v.ensureToken(ctx, native); errToken != nil {
		return nil, errToken
	}
	payload, errChat := Chat(ctx, v.gatewayHost, native.BearerToken(), body)
	if errChat == nil {
		return payload, nil
	}
	if !v.retryOn401 || !isUnauthorized(errChat) {
		return nil, core.ToPluginError(errChat)
	}
	if errRefresh := v.forceRefreshToken(ctx, cred, native); errRefresh != nil {
		return nil, core.ToPluginError(errChat)
	}
	payload, errChat = Chat(ctx, v.gatewayHost, native.BearerToken(), body)
	if errChat != nil {
		return nil, core.ToPluginError(errChat)
	}
	return payload, nil
}

// forceRefreshToken 强制换一次 token（忽略本地过期时间）。
func (v *adapter) forceRefreshToken(ctx context.Context, cred *core.Credential, native *Credential) error {
	cookie := native.CookieValue()
	token, expires, errFetch := FetchToken(ctx, v.webBase, cookie)
	if errFetch != nil {
		return errFetch
	}
	v.storeToken(cookie, cachedToken{token: token, expires: expires})
	native.Token = token
	native.ExpiresAt = expires
	// 同步回 core.Credential，让同一进程内的其它路径看到新票（写回文件由
	// 宿主的续期钩子在下一轮触发）。
	if cred != nil {
		cred.ApplyTokenRefresh(token, "", expires)
	}
	logger.Debug("dumate：票据被上游提前作废，已强刷")
	return nil
}

// isUnauthorized 报告错误是否为 401/403。
func isUnauthorized(err error) bool {
	failure, okFailure := err.(*core.Failure)
	if !okFailure || failure == nil {
		return false
	}
	return failure.Status == http.StatusUnauthorized || failure.Status == http.StatusForbidden
}

// ConfigFields 声明搭子专属配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "dumate_web_base", Type: pluginapi.ConfigFieldTypeString, Description: "网页端基地址（取 token、签到、积分），默认 https://www.dumate.cn"},
		{Name: "dumate_gateway_host", Type: pluginapi.ConfigFieldTypeString, Description: "模型网关地址，默认 https://dumate-svc.baidu.com"},
	}
}

// NewCredentialJSON 构造一份可供用户导入的标准凭证 JSON。
//
// 导出这个函数是为了让根层（或控制台的「添加账号」提示）能生成与解析器
// 完全一致的样本，避免文档里写的字段名与代码里认的字段名长期分叉。
func NewCredentialJSON(cookie, label string) ([]byte, error) {
	cookie = normalizeCookie(cookie)
	if cookie == "" {
		return nil, fmt.Errorf("cookie is empty")
	}
	cred := &Credential{Cookie: cookie, Label: strings.TrimSpace(label)}
	return MergeStorageJSON(nil, cred)
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.AuthModeReporter     = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.Refresher            = (*adapter)(nil)
	_ core.StorageMerger        = (*adapter)(nil)
	_ core.CheckinProvider      = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
)
