package dumate

// 本文件实现百度搭子（DuMate）凭证的解析、归属判定与落盘合并。
//
// 搭子有两种凭证来源：
//   1. 网页凭证：浏览器 Cookie（BDUSS 等），GET /api/dumate/app/token/get
//      换一个约 1 小时有效的 gateway token，再打 dumate-svc.baidu.com。
//      优点：可多账号、token 自动续；缺点：Cookie 会随浏览器退出/改密码失效。
//   2. 桌面凭证：本机 8980 的登录态（auth.json）。本插件**不走这条**——
//      桌面链路同一时刻只有一份登录态，且必须是 Windows 客户端，
//      无法在 Linux 插件进程里多账号并存。
//
// 因此本包只认网页 Cookie，凭证 JSON 形如：
//
//	{
//	  "vendor": "dumate",
//	  "type": "freetier",
//	  "cookie": "BDUSS=...; STOKEN=...; ...",
//	  "token": "<gateway token，自动刷新后写回>",
//	  "expires_at": 1790000000,
//	  "label": "我的搭子"
//	}
//
// cookie 是长期凭据，token 是短期票据：token 丢了能再换，cookie 丢了只能重登。

import (
	"encoding/json"
	"fmt"
	"strings"

	"freetier2api-plugin/internal/core"
)

const (
	// VendorID 是供应商实例标识，同时是凭证文件名前缀（dumate-<uid>.json）。
	VendorID = "dumate"
	// VendorName 是管理端展示名。
	VendorName = "百度搭子"
)

// Credential 是归一化后的搭子凭证。
type Credential struct {
	// Cookie 是网页登录态（长期凭据，出站时原样放进 Cookie 头）。
	Cookie string
	// Token 是云端网关票据（短期，约 1 小时）。
	Token string
	// ExpiresAt 是 Token 的过期时刻（Unix 秒）；0 表示未知。
	ExpiresAt int64
	// UID 是上游账号标识（用于展示与去重，可空）。
	UID string
	// Nickname 是昵称（展示用）。
	Nickname string
	// Label 是展示名。
	Label string
}

// BearerToken 返回可放进 Authorization 头的票据。
func (c *Credential) BearerToken() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Token)
}

// CookieValue 返回 Cookie 头的值。
func (c *Credential) CookieValue() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Cookie)
}

// 凭证里可能承载 cookie 的字段名。
//
// 多候选是刻意的：用户从浏览器复制、从别家 2api 导入、手写文件时写法各异。
var credentialCookieKeys = []string{
	"cookie", "Cookie", "cookies", "COOKIE",
	// 兼容管理端通用「添加账号」表单：它只提交 api_key 字段。搭子没有
	// 官方 API Key，用户在该框里填的实际就是 Cookie 串，按 Cookie 处理。
	"api_key", "apiKey",
}

// 凭证里可能承载 token 的字段名。
var credentialTokenKeys = []string{
	"token", "access_token", "accessToken", "gw_token",
}

// ParseCredential 解析搭子凭证 JSON。
//
// 除标准 JSON 外，还兼容「直接把浏览器里那行 Cookie 粘进文件」的形态：
// raw 不以 { 开头时按原始 Cookie 串处理。这条路径很重要——让用户手写
// 一份 {"cookie": "BDUSS=...; ..."} 需要他先懂 JSON 转义，失败率高得多。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, fmt.Errorf("credential is empty")
	}
	if !strings.HasPrefix(text, "{") {
		cookie := normalizeCookie(extractCookieSegment(text))
		if cookie == "" {
			return nil, fmt.Errorf("credential is neither JSON nor a cookie string")
		}
		// 用户最常复制的是 BDUSS 的**值**本身（DevTools 里那一长串），
		// 没有 `BDUSS=` 前缀也没有分号。这里补上，避免他必须懂 Cookie 语法。
		if !strings.Contains(cookie, "=") {
			if !looksLikeBDUSS(cookie) {
				return nil, fmt.Errorf("无法识别的凭证：既不是 Cookie，也不像 BDUSS")
			}
			cookie = "BDUSS=" + cookie
		}
		return &Credential{Cookie: cookie}, nil
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{
		Cookie:   normalizeCookie(firstString(payload, credentialCookieKeys...)),
		Token:    firstString(payload, credentialTokenKeys...),
		UID:      firstString(payload, "uid", "userId", "user_id"),
		Nickname: firstString(payload, "nickname", "nickName", "displayName", "name"),
		Label:    firstString(payload, "label"),
	}
	cred.ExpiresAt = parseUnix(payload["expires_at"], payload["expiresAt"], payload["expired_at"])
	// 面板把输入框内容原样塞进 cookie 字段，用户填的很可能是裸 BDUSS 值。
	if cred.Cookie != "" && !strings.Contains(cred.Cookie, "=") && looksLikeBDUSS(cred.Cookie) {
		cred.Cookie = "BDUSS=" + cred.Cookie
	}
	// 只有 BDUSS 也能工作：账号识别靠它，缺 STOKEN 会少部分能力但能对话。
	if cred.Cookie == "" {
		if bduss := firstString(payload, "bduss", "BDUSS"); bduss != "" {
			cred.Cookie = "BDUSS=" + strings.TrimSpace(bduss)
		}
	}
	if cred.Cookie == "" {
		return nil, fmt.Errorf("credential has no cookie (tried: %s)", strings.Join(credentialCookieKeys, ", "))
	}
	return cred, nil
}

// extractCookieSegment 从「整段请求头 / cURL 命令 / 单行 Cookie」里抠出 Cookie 值。
//
// 面板上用户可能粘三种东西：
//
//	BDUSS=xxx; STOKEN=yyy            ← 直接是 Cookie
//	Cookie: BDUSS=xxx; STOKEN=yyy    ← DevTools 请求头里复制的一行
//	curl 'https://…' -H 'Cookie: …'  ← 「Copy as cURL」
//
// 全部归一成第一种形态；认不出来就原样返回，交给 normalizeCookie 兜底。
func extractCookieSegment(text string) string {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	idx := strings.Index(lower, "cookie:")
	if idx < 0 {
		return trimmed
	}
	rest := trimmed[idx+len("cookie:"):]
	// 引号可能是 cURL 的 '…' 或 "…"，也可能是复制时带上的多余空格。
	// 只按引号截断：换行在 Cookie 里是合法的续行，交给 normalizeCookie 折叠。
	rest = strings.TrimLeft(rest, " \t'\"")
	if end := strings.IndexAny(rest, "'\""); end >= 0 {
		rest = rest[:end]
	}
	// cURL 里同一行后面还跟着别的参数，截到下一个 -H 之前。
	if cut := strings.Index(rest, " -H "); cut > 0 {
		rest = rest[:cut]
	}
	rest = strings.TrimRight(rest, " \\\t")
	if !strings.Contains(rest, "=") {
		return trimmed
	}
	return rest
}

// looksLikeBDUSS 判断一串裸值是否像 BDUSS。
//
// BDUSS 是 URL-safe base64，长度通常 150–250 字符，只含字母数字与 - _。
// 长度下限给 40 是为了不把「用户误填的短字符串」当成 BDUSS 存进去。
func looksLikeBDUSS(value string) bool {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < 40 || len(trimmed) > 512 {
		return false
	}
	for _, char := range trimmed {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '-' || char == '_':
		default:
			return false
		}
	}
	return true
}

// normalizeCookie 清理 Cookie 串：去掉换行、结尾分号、`Cookie:` 前缀。
//
// 从浏览器「Copy as cURL」或 DevTools 头部视图复制时，常见三种污染：
// 带 `Cookie: ` 前缀、跨行、末尾多一个分号。这些都会让上游解析出空值。
func normalizeCookie(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	// 去 `Cookie:` / `cookie:` 前缀
	if idx := strings.IndexByte(text, ':'); idx > 0 && strings.EqualFold(strings.TrimSpace(text[:idx]), "cookie") {
		text = text[idx+1:]
	}
	// 多行折叠成单行
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	// 连续空白归一
	fields := strings.Fields(text)
	text = strings.Join(fields, " ")
	// 去末尾分号
	text = strings.TrimRight(strings.TrimSpace(text), ";")
	text = strings.TrimSpace(text)
	return text
}

// LooksLikeCredential 判断一份 JSON 是否属于搭子。
//
// **判定必须偏严**：宿主会把所有非内建格式的凭证依次喂给每个插件，误吞
// 别家的会让宿主用本插件的结构覆盖对方账号（且是静默的）。
//
// 接受三类证据（任一成立）：
//  1. 显式 vendor="dumate"；
//  2. 文件名含 dumate 段（用户添加时就是这个名字）；
//  3. 有 cookie 字段，且带搭子独有的域名/字段线索。
//
// 刻意不认「只有 cookie 字段」的裸凭证：Qoder/Trae 等也可能存 cookie，
// 拿它当判据必然跨供应商误吞。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if decided, claimed := core.VendorFieldVerdict(raw, VendorID); decided {
		return claimed
	}
	if core.FileNameMatchesConvention(fileName, VendorID) {
		return true
	}
	if raw == nil {
		return false
	}
	if hasDumateMarker(raw) {
		return true
	}
	_ = provider
	return false
}

// hasDumateMarker 报告凭证里是否带搭子独有的线索。
func hasDumateMarker(raw map[string]any) bool {
	for _, key := range []string{"dumate", "dumate_cookie", "dumateCookie", "gw_token"} {
		if _, exists := raw[key]; exists {
			return true
		}
	}
	// Cookie 里出现搭子域名的，几乎只能是搭子凭证。
	for _, key := range credentialCookieKeys {
		value, okValue := raw[key].(string)
		if !okValue {
			continue
		}
		lower := strings.ToLower(value)
		if strings.Contains(lower, "dumate.cn") || strings.Contains(lower, "dumate.baidu.com") {
			return true
		}
	}
	return false
}

// MergeStorageJSON 把刷新后的 token 合并回原凭证 JSON。
//
// 以原文件为基础做增量合并：用户在文件里手写的 label / note / disabled
// 等字段不能因为一次 token 刷新就被抹掉。
func MergeStorageJSON(original []byte, cred *Credential) ([]byte, error) {
	if cred == nil {
		return original, nil
	}
	payload := map[string]any{}
	text := strings.TrimSpace(string(original))
	if strings.HasPrefix(text, "{") {
		if errUnmarshal := json.Unmarshal(original, &payload); errUnmarshal != nil {
			payload = map[string]any{}
		}
	}
	// 原始 Cookie 串形态：合并时升级成标准 JSON，保留 cookie 本体。
	//
	// 只在「新值更完整」时覆盖：用户可能只填了 BDUSS，插件补全成整套 Cookie
	// 后需要把结果写回文件（否则每次续期都要重跑一次兑换）；但如果新值反而
	// 比文件里的短，说明是解析降级，保留原值。
	if cred.Cookie != "" {
		existing, _ := payload["cookie"].(string)
		if strings.TrimSpace(existing) == "" || len(cred.Cookie) > len(existing) {
			payload["cookie"] = cred.Cookie
		}
	}
	if payload["vendor"] == nil {
		payload["vendor"] = VendorID
	}
	if payload["type"] == nil {
		payload["type"] = "freetier"
	}
	if cred.Token != "" {
		payload["token"] = cred.Token
	}
	if cred.ExpiresAt > 0 {
		payload["expires_at"] = cred.ExpiresAt
	}
	if cred.UID != "" {
		payload["uid"] = cred.UID
	}
	if cred.Nickname != "" {
		payload["nickname"] = cred.Nickname
	}
	encoded, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return original, errMarshal
	}
	return encoded, nil
}

// firstString 返回第一个非空字符串字段。
func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// parseUnix 解析各种时间戳写法（int64 / float64 / 数字字符串），返回 Unix 秒。
//
// 上游 expired_at 是 RFC3339 字符串（2026-11-15T19:38:34Z），
// 本地写入的是 Unix 秒；两种都要认。
func parseUnix(values ...any) int64 {
	for _, value := range values {
		switch typed := value.(type) {
		case float64:
			if typed > 0 {
				return int64(typed)
			}
		case int64:
			if typed > 0 {
				return typed
			}
		case json.Number:
			if parsed, errParse := typed.Int64(); errParse == nil && parsed > 0 {
				return parsed
			}
		case string:
			trimmed := strings.TrimSpace(typed)
			if trimmed == "" {
				continue
			}
			if parsed, okTime := parseTimeString(trimmed); okTime {
				return parsed
			}
		}
	}
	return 0
}
