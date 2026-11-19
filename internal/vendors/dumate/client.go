package dumate

// 本文件实现百度搭子的上游端点、请求头与 token 换取。
//
// 链路（2026-11-15 真机实测，来源 wearetheone777/dumate2api 与
// R227BOUT/magpie-dumate 的实测记录）：
//
//	GET  https://www.dumate.cn/api/dumate/app/token/get
//	     Cookie: <网页登录态>
//	     → { "result": { "token": "...", "expired_at": "2026-11-15T19:38:34Z" } }
//	POST https://dumate-svc.baidu.com/gateway/apis/v1/chat/completions
//	     Authorization: Bearer <token>
//	     X-Dumate-Client-Type: web
//	     X-Dumate-Account-Id: global
//
// token 有效期约 1 小时，同一 Cookie 在过期前重复取会拿到同一个 token
// （服务端缓存）。两个签发域（www.dumate.cn / console.bce.baidu.com）是同一
// 服务的两条入口，token 与域无关，因此只在「该域不通」时才换域。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

const (
	// DefaultWebBase 是网页端基地址（取 token、签到、积分都打这里）。
	DefaultWebBase = "https://www.dumate.cn"
	// FallbackWebBase 是备用签发域（同一签发服务，token 与域无关）。
	FallbackWebBase = "https://console.bce.baidu.com"
	// TokenPath 是换取 gateway token 的路径。
	TokenPath = "/api/dumate/app/token/get"
	// DefaultGatewayHost 是模型网关。
	DefaultGatewayHost = "https://dumate-svc.baidu.com"
	// GatewayChatPath 是对话路径（OpenAI 兼容）。
	GatewayChatPath = "/gateway/apis/v1/chat/completions"

	// UserInfoPath 是账号信息（昵称/uid）。
	UserInfoPath = "/api/dumate/user/info"
	// QuotaPath 是积分总览。
	QuotaPath = "/api/dumate/points/quota_overview"
	// LoginBonusInfoPath 是签到状态。
	LoginBonusInfoPath = "/api/dumate/points/loginBonusInfo"
	// LoginBonusPath 是签到领取。
	LoginBonusPath = "/api/dumate/points/loginBonus"

	// DefaultUserAgent 是伪装成浏览器的 UA。
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
)

// refreshSkew 是 token 提前刷新的余量：留 5 分钟，避免请求刚好压在过期点上。
const refreshSkew = 5 * time.Minute

// WebBase 返回网页端基地址（末尾无斜杠）。允许配置覆盖。
func WebBase(override string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return DefaultWebBase
	}
	return trimmed
}

// GatewayHost 返回模型网关基地址（末尾无斜杠）。允许配置覆盖。
func GatewayHost(override string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return DefaultGatewayHost
	}
	return trimmed
}

// tokenHosts 返回取 token 的候选域（首个是配置的 base，其余是容灾）。
func tokenHosts(webBase string) []string {
	primary := WebBase(webBase)
	hosts := []string{primary}
	// 只有生产域才追加容灾域：用户把 base 指到自建/测试环境时，回落到
	// console.bce.baidu.com 等于把测试环境的 Cookie 发给生产租户。
	if strings.Contains(primary, "dumate.cn") {
		hosts = append(hosts, FallbackWebBase)
	}
	return hosts
}

// webHeaders 构造网页端 API 的请求头（Cookie 认证）。
//
// 网页端校验 X-Dumate-Client-Type 与 Referer：缺了会被当成非浏览器来源
// 直接拒绝，因此这几个头是硬要求而非装饰。
func webHeaders(req *http.Request, cookie string) {
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("X-Dumate-Client-Type", "web")
	req.Header.Set("Referer", DefaultWebBase+"/app")
	req.Header.Set("Origin", DefaultWebBase)
	req.Header.Set("User-Agent", DefaultUserAgent)
}

// gatewayHeaders 构造模型网关的请求头（Bearer token）。
func gatewayHeaders(req *http.Request, token string, stream bool) {
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+token)
	// 与桌面客户端一致：标记来源与账号域（缺了网关按匿名处理，部分模型 403）。
	req.Header.Set("X-Dumate-Client-Type", "web")
	req.Header.Set("X-Dumate-Account-Id", "global")
	req.Header.Set("User-Agent", DefaultUserAgent)
}

// webRequest 执行一次网页端 API 调用，返回原始响应体与状态码。
func webRequest(ctx context.Context, webBase, method, path, cookie string, body []byte, timeout time.Duration) (int, []byte, error) {
	endpoint := WebBase(webBase) + path
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, errReq := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if errReq != nil {
		return 0, nil, fmt.Errorf("build request: %w", errReq)
	}
	webHeaders(req, cookie)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, errDo := hostbridge.Client(ctx, timeout).Do(req)
	if errDo != nil {
		return 0, nil, &core.Failure{Msg: "dumate web: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()
	payload, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if errRead != nil {
		return resp.StatusCode, nil, &core.Failure{Status: resp.StatusCode, Msg: "read response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	return resp.StatusCode, payload, nil
}

// tokenResponse 是取 token 的响应体。
type tokenResponse struct {
	Result struct {
		Token     string `json:"token"`
		ExpiredAt string `json:"expired_at"`
	} `json:"result"`
	Message string `json:"message"`
	Msg     string `json:"msg"`
	Code    int    `json:"code"`
}

// FetchToken 用 Cookie 换一个 gateway token。
//
// 返回过期时刻（Unix 秒）；上游给的是 ISO 时间，解析失败时按 50 分钟兜底
// ——宁可早刷，不要晚刷（晚刷意味着请求已经带着死票出门）。
func FetchToken(ctx context.Context, webBase, cookie string) (string, int64, error) {
	if strings.TrimSpace(cookie) == "" {
		return "", 0, core.NewPluginError("credential_missing", "dumate 凭证没有 cookie", http.StatusUnauthorized)
	}
	var lastStatus int
	var lastBody []byte
	var lastErr error
	for _, host := range tokenHosts(webBase) {
		status, payload, errDo := webRequest(ctx, host, http.MethodGet, TokenPath, cookie, nil, 20*time.Second)
		if errDo != nil {
			lastStatus, lastErr = 0, errDo
			continue
		}
		lastStatus, lastBody = status, payload
		if status == http.StatusOK {
			var parsed tokenResponse
			if errUnmarshal := json.Unmarshal(payload, &parsed); errUnmarshal == nil && parsed.Result.Token != "" {
				expires := parseExpiredAt(parsed.Result.ExpiredAt)
				if expires == 0 {
					expires = time.Now().Add(50 * time.Minute).Unix()
				}
				// 票据到期时间只存在于内存与这条日志里（凭证文件不缓存 token），
				// 所以每次换票都必须在日志里留一行：用户问「什么时候过期」、
				// 排查「到期后有没有自动续」，都要靠它。
				logger.Info("dumate：已换取网关票据，有效期至 %s（剩余 %.1f 小时）",
					time.Unix(expires, 0).Format("2006-01-02 15:04:05"),
					time.Until(time.Unix(expires, 0)).Hours())
				return parsed.Result.Token, expires, nil
			}
			// 2xx 但不是 JSON：被墙/被跳登录页，换域还有意义。
			lastErr = fmt.Errorf("token response is not JSON")
			continue
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			// 先分清是「凭证不全」还是「凭证过期」：这两种在用户看来都是
			// 「登录已经过期」，但一个要补步骤、一个要重新登录，提示必须分开，
			// 否则用户会反复重新登录却始终缺同一个字段。
			if !HasConsoleSession(cookie) {
				return "", 0, core.NewPluginError("credential_incomplete",
					"Cookie 里缺少 "+SessionCookieName+"（console 域长会话）。请先打开 "+
						ConsoleURL+" 让百度种下它，再在那里复制整行 Cookie。"+
						"只登录百度账号页不会产生这个字段，缺它一定报「登录已经过期」。",
					http.StatusUnauthorized)
			}
			return "", 0, core.NewPluginError("credential_invalid",
				"搭子登录态已失效，请重新打开 "+WebLoginURL+" 登录，再复制整行 Cookie", http.StatusUnauthorized)
		}
		// 其余状态（4xx/429）换域不会变好，直接报。
		break
	}
	if lastStatus == 0 && lastErr != nil {
		return "", 0, lastErr
	}
	return "", 0, Classify(lastStatus, string(lastBody))
}

// parseExpiredAt 解析上游的 ISO 过期时间。
func parseExpiredAt(raw string) int64 {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05"} {
		if parsed, errParse := time.Parse(layout, trimmed); errParse == nil {
			return parsed.Unix()
		}
	}
	return 0
}

// parseTimeString 解析各种时间戳字符串（供凭证解析用）。
func parseTimeString(raw string) (int64, bool) {
	if unix, errUnix := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); errUnix == nil {
		if unix > 1_000_000_000 {
			return unix, true
		}
		// 毫秒时间戳
		if unix > 1_000_000_000_000 {
			return unix / 1000, true
		}
		return 0, false
	}
	if parsed := parseExpiredAt(raw); parsed > 0 {
		return parsed, true
	}
	return 0, false
}

// ensureToken 保证凭证持有可用 token，必要时现场刷新。
//
// 判断顺序刻意是「先看内存缓存，再看本地票」——每个对话请求都打一次
// token/get 会把上游打爆，而 token/get 本身也是被限流的。宿主不会把
// token 写回文件，所以「本地票」在这里通常是空的，缓存才是真正起作用的一层。
func (v *adapter) ensureToken(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return core.NewPluginError("credential_missing", "credential is nil", http.StatusUnauthorized)
	}
	if token := cred.BearerToken(); token != "" && cred.ExpiresAt > time.Now().Add(refreshSkew).Unix() {
		return nil
	}
	cookie := cred.CookieValue()
	if hit := v.cachedTokenFor(cookie); hit.token != "" {
		cred.Token = hit.token
		cred.ExpiresAt = hit.expires
		return nil
	}
	next, expires, errFetch := FetchToken(ctx, v.webBase, cookie)
	if errFetch != nil {
		return errFetch
	}
	cred.Token = next
	cred.ExpiresAt = expires
	v.storeToken(cookie, cachedToken{token: next, expires: expires})
	return nil
}
