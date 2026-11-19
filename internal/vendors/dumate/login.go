package dumate

// 本文件实现搭子账号的「网页登录」接入。
//
// 早期版本走百度扫码登录（getqrcode → channel/unicast 轮询 → qrbdusslogin）。
// 实测不可用：二维码由服务器（机房 IP、无 BAIDUID 会话）取出，用户手机
// 扫上去后百度侧直接判失效，扫码框里的图往往一打开就过期。因此**不再提供扫码**。
//
// 现在的做法是「官方登录页 + 登录态回填」：
//
//	1. 面板按钮直接打开 https://login.bce.baidu.com（百度统一登录，账号密码 /
//	   短信 / 手机端扫码都行，全是百度自己的页面，信任链不经过本插件）；
//	2. 用户登录成功后，从浏览器 DevTools 里复制 BDUSS（或整行 Cookie）粘回面板；
//	3. 插件用 BDUSS 调 passport 的 qrbdusslogin 换取全套 Cookie（尽力而为：
//	   换不到就直接用 BDUSS，token/get 只需 BDUSS 即可工作）。
//
// 之所以必须让用户手工复制一次：BDUSS 是 HttpOnly Cookie，任何网页脚本
// （包括本面板）都读不到，跨域也取不到；后端代登录则要经手用户的百度密码，
// 这是不能做的。一次复制换长期免登录，是当前唯一安全且可靠的路径。

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

const (
	// passportBase 是百度统一登录域。
	passportBase = "https://passport.baidu.com"
	// WebLoginURL 是百度统一登录页（同时是智能云 BCE 的登录入口）。
	//
	// 用户在这个页面里完成登录：账号密码、短信验证码、手机百度 App 扫码都可以，
	// 因为页面本身就是百度官方的，本插件不接触任何密码。
	WebLoginURL = "https://login.bce.baidu.com/?redirect=" +
		"https%3A%2F%2Fconsole.bce.baidu.com%2F"
	// ConsoleURL 是搭子自己的控制台页。
	//
	// **登录完必须再打开它一次**：调 /api/dumate/* 要的是 console 域的会话
	// cookie（bce-long-term-sessionid），它只在浏览器真正访问过该域后才由
	// 服务端种下。只登 passport 不产生它，缺了它所有接口一律 401
	// 「登录已经过期」——这正是长期困扰用户的那个假象：BDUSS 明明有效却全挂。
	ConsoleURL = "https://console.bce.baidu.com/ai/#/app/dumate/overview"
	// SessionCookieName 是 console 域的长会话 cookie 名。
	SessionCookieName = "bce-long-term-sessionid"
	// qrLoginTimeout 是单次上游请求上限。
	qrLoginTimeout = 25 * time.Second
)

// LoginStart 返回百度统一登录页地址。
//
// 刻意不生成二维码、不登记轮询会话：state 留空，面板据此只展示「打开登录页」
// 而不会去轮询一个永远不会成功的状态。
func (v *adapter) LoginStart(_ context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return &pluginapi.AuthLoginStartResponse{
		Provider: core.ProviderKey,
		URL:      WebLoginURL,
		Metadata: map[string]any{
			core.VendorKey: VendorID,
			"login_mode":   "web",
			"hint":         "在打开的页面完成百度登录，回到面板粘贴 BDUSS 或整行 Cookie",
		},
	}, nil
}

// LoginPoll 不支持轮询：登录发生在用户自己的浏览器里，插件无法感知。
//
// 保留实现只是为了满足 core.LoginProvider 接口，调用方拿到明确指引即可。
func (v *adapter) LoginPoll(_ context.Context, _ string) (*pluginapi.AuthLoginPollResponse, error) {
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusError,
		Message: "搭子不支持轮询登录：请在百度登录页完成登录后，把 BDUSS 或整行 Cookie 粘贴到「添加账号」里",
	}, nil
}

// ExchangeBDUSS 用 BDUSS 兑换一整套 Cookie（BDUSS / STOKEN / PTOKEN / BAIDUID…）。
//
// 这是百度 passport 的「凭 bduss 免扫码登录」接口，扫码登录的最后一跳也是它，
// 只是当时喂的是二维码轮询拿到的临时 bduss。用户从浏览器复制的正式 BDUSS
// 同样能走通，因此这里直接复用。
//
// 该接口以 302 + Set-Cookie 下发登录态，因此**不能跟随重定向**：一旦跟随，
// 客户端会带着 cookie 跳到 www.baidu.com，我们反而读不到原始响应头里的
// Set-Cookie（Go 会把它们吃进 jar）。
func ExchangeBDUSS(ctx context.Context, bduss string) (string, error) {
	bduss = strings.TrimSpace(bduss)
	if bduss == "" {
		return "", fmt.Errorf("bduss is empty")
	}
	var lastErr error
	// 两组参数：qrcode=1 是扫码来源标记，个别账号只认不带该标记的请求。
	for _, withQRCode := range []bool{true, false} {
		cookie, err := exchangeBDUSSOnce(ctx, bduss, withQRCode)
		if err == nil && cookie != "" {
			return cookie, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("上游没有下发 BDUSS")
	}
	return "", lastErr
}

// exchangeBDUSSOnce 执行一次兑换请求。
func exchangeBDUSSOnce(ctx context.Context, bduss string, withQRCode bool) (string, error) {
	ms := time.Now().UnixMilli()
	endpoint := fmt.Sprintf("%s/v3/login/main/qrbdusslogin?bduss=%s&v=%d&u=%s&tpl=mn&apiver=v3&tt=%d",
		passportBase, url.QueryEscape(bduss), ms, url.QueryEscape("https://www.baidu.com/"), ms)
	if withQRCode {
		endpoint += "&qrcode=1"
	}

	client := &http.Client{
		Timeout: qrLoginTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return "", errRequest
	}
	applyPassportHeaders(request)
	response, errDo := client.Do(request)
	if errDo != nil {
		return "", fmt.Errorf("兑换登录态失败：%w", errDo)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))

	cookies := map[string]string{}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "" || cookie.Value == "" {
			continue
		}
		cookies[cookie.Name] = cookie.Value
	}
	if cookies["BDUSS"] == "" {
		return "", fmt.Errorf("上游没有下发 BDUSS（HTTP %d），请重试", response.StatusCode)
	}
	// 顺序不影响服务端解析；固定顺序便于比对日志。
	order := []string{"BDUSS", "BDUSS_BFESS", "STOKEN", "PTOKEN", "BAIDUID", "BAIDUID_BFESS"}
	parts := make([]string, 0, len(cookies))
	seen := map[string]bool{}
	for _, name := range order {
		if value, okValue := cookies[name]; okValue {
			parts = append(parts, name+"="+value)
			seen[name] = true
		}
	}
	for name, value := range cookies {
		if !seen[name] {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, "; "), nil
}

// BDUSSFromCookie 从 Cookie 串里取 BDUSS 值（没有则返回空）。
func BDUSSFromCookie(cookie string) string {
	return CookieField(cookie, "BDUSS")
}

// HasConsoleSession 报告 Cookie 里是否带 console 域的长会话。
//
// 这是「能用的凭证」和「看起来完整但一定 401」的分水岭：用户从控制台复制
// 的 Cookie 往往有三十来个字段（BDUSS、bce-session、bce-user-info…），唯独
// 缺 bce-long-term-sessionid，于是每次调用都报「登录已经过期」，而账号本身
// 明明是好的。加这一层判断，是为了把这句话翻译成「你少走了一步」。
func HasConsoleSession(cookie string) bool {
	return CookieField(cookie, SessionCookieName) != ""
}

// CookieField 取 Cookie 串里指定字段的值。
func CookieField(cookie, name string) string {
	for _, segment := range strings.Split(cookie, ";") {
		segment = strings.TrimSpace(segment)
		if idx := strings.IndexByte(segment, '='); idx > 0 {
			if strings.EqualFold(strings.TrimSpace(segment[:idx]), name) {
				return strings.TrimSpace(segment[idx+1:])
			}
		}
	}
	return ""
}

// passportGet 发一次带浏览器头的 GET，返回响应体。
func passportGet(ctx context.Context, endpoint string, extra http.Header) ([]byte, error) {
	client := &http.Client{Timeout: qrLoginTimeout}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	applyPassportHeaders(request)
	for name, values := range extra {
		for _, value := range values {
			request.Header.Set(name, value)
		}
	}
	response, errDo := client.Do(request)
	if errDo != nil {
		return nil, errDo
	}
	defer response.Body.Close()
	return io.ReadAll(io.LimitReader(response.Body, 512<<10))
}

// applyPassportHeaders 设置百度要求的浏览器头。
//
// UA 必须是桌面浏览器：2020 年后 passport 对非浏览器 UA 会返回异常页面。
func applyPassportHeaders(request *http.Request) {
	request.Header.Set("User-Agent", DefaultUserAgent)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
}

// decodeJSONP 解析 `cb({...})` 或裸 JSON。
func decodeJSONP(body []byte) (map[string]any, error) {
	text := strings.TrimSpace(string(body))
	if start := strings.IndexByte(text, '('); start >= 0 {
		if end := strings.LastIndexByte(text, ')'); end > start {
			text = strings.TrimSpace(text[start+1 : end])
		}
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal([]byte(text), &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return payload, nil
}

// isTimeout 判断错误是否为超时（长轮询未扫码的正常表现）。
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if osErr, okTimeout := err.(interface{ Timeout() bool }); okTimeout && osErr.Timeout() {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout") ||
		strings.Contains(strings.ToLower(err.Error()), "deadline exceeded")
}

// randomUUID 生成 RFC 4122 v4 UUID（百度按 uuid 格式解析 gid）。
func randomUUID() string {
	buffer := make([]byte, 16)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return core.RandomHex(16)
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		buffer[0:4], buffer[4:6], buffer[6:8], buffer[8:10], buffer[10:16])
}

// safePrefix 截取前 n 个字符用于日志（票据不进日志全文）。
func safePrefix(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n]
}
