package dumate

// 本文件覆盖三条最容易出错的逻辑：凭证解析（三种真实写法）、
// 跨供应商误吞防护、请求体的模型名映射。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseCredentialFormats(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "标准 JSON",
			raw:  `{"vendor":"dumate","cookie":"BDUSS=abc; STOKEN=def","token":"tk","expires_at":1790000000,"label":"我的搭子"}`,
			want: "BDUSS=abc; STOKEN=def",
		},
		{
			name: "裸 Cookie 串（用户直接粘贴）",
			raw:  "BDUSS=abc; STOKEN=def;",
			want: "BDUSS=abc; STOKEN=def",
		},
		{
			name: "带 Cookie: 前缀且跨行",
			raw:  "Cookie: BDUSS=abc;\r\n  STOKEN=def ;",
			want: "BDUSS=abc; STOKEN=def",
		},
		{
			name: "只有 BDUSS 字段",
			raw:  `{"bduss":"abc"}`,
			want: "BDUSS=abc",
		},
		{
			name: "裸 BDUSS 值（DevTools 里复制的那一长串）",
			raw:  "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_-",
			want: "BDUSS=abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_-",
		},
		{
			name: "面板包成 JSON 的裸 BDUSS",
			raw:  `{"vendor":"dumate","cookie":"abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_-"}`,
			want: "BDUSS=abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_-",
		},
		{
			name: "Copy as cURL",
			raw:  `curl 'https://passport.baidu.com/v2/api/?login' -H 'Cookie: BDUSS=abc; STOKEN=def' -H 'User-Agent: x'`,
			want: "BDUSS=abc; STOKEN=def",
		},
		{
			name:    "既不是 Cookie 也不像 BDUSS",
			raw:     "abc",
			wantErr: true,
		},
		{
			name:    "空对象应报错",
			raw:     `{"label":"x"}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cred, errParse := ParseCredential([]byte(tc.raw))
			if tc.wantErr {
				if errParse == nil {
					t.Fatalf("expected error, got %+v", cred)
				}
				return
			}
			if errParse != nil {
				t.Fatalf("unexpected error: %v", errParse)
			}
			if cred.Cookie != tc.want {
				t.Fatalf("cookie = %q, want %q", cred.Cookie, tc.want)
			}
		})
	}
}

func TestParseCredentialKeepsToken(t *testing.T) {
	cred, errParse := ParseCredential([]byte(`{"cookie":"BDUSS=x","token":"tk-1","expires_at":1790000000}`))
	if errParse != nil {
		t.Fatalf("unexpected error: %v", errParse)
	}
	if cred.BearerToken() != "tk-1" {
		t.Fatalf("token = %q", cred.BearerToken())
	}
	if cred.ExpiresAt != 1790000000 {
		t.Fatalf("expires = %d", cred.ExpiresAt)
	}
}

func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		raw      map[string]any
		want     bool
	}{
		{
			name:     "文件名约定",
			fileName: "dumate-123456.json",
			raw:      map[string]any{"cookie": "BDUSS=x"},
			want:     true,
		},
		{
			name:     "显式 vendor 字段",
			fileName: "whatever.json",
			raw:      map[string]any{"vendor": "dumate", "cookie": "BDUSS=x"},
			want:     true,
		},
		{
			name:     "cookie 里带搭子域名",
			fileName: "whatever.json",
			raw:      map[string]any{"cookie": "BDUSS=x; domain=.dumate.cn"},
			want:     true,
		},
		{
			name:     "别家凭证（workbuddy 嵌套结构）",
			fileName: "workbuddycn-abc.json",
			raw:      map[string]any{"auth": map[string]any{"access_token": "x"}},
			want:     false,
		},
		{
			name:     "声明了别家 vendor",
			fileName: "whatever.json",
			raw:      map[string]any{"vendor": "cline", "cookie": "x"},
			want:     false,
		},
		{
			name:     "只有 cookie 字段、无任何搭子线索",
			fileName: "whatever.json",
			raw:      map[string]any{"cookie": "BDUSS=x"},
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeCredential(tc.raw, tc.fileName, "freetier"); got != tc.want {
				t.Fatalf("LooksLikeCredential = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMergeStorageJSONKeepsUserFields(t *testing.T) {
	original := []byte(`{"vendor":"dumate","cookie":"BDUSS=x","note":"我手写的备注","disabled":false}`)
	merged, errMerge := MergeStorageJSON(original, &Credential{Cookie: "BDUSS=x", Token: "tk-2", ExpiresAt: 1800000000})
	if errMerge != nil {
		t.Fatalf("unexpected error: %v", errMerge)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(merged, &payload); errUnmarshal != nil {
		t.Fatalf("merged output is not JSON: %v", errUnmarshal)
	}
	if payload["note"] != "我手写的备注" {
		t.Fatalf("用户字段被抹掉: %v", payload["note"])
	}
	if payload["token"] != "tk-2" {
		t.Fatalf("token 未写回: %v", payload["token"])
	}
}

func TestPrepareBodyMapsModelAndUsage(t *testing.T) {
	body := []byte(`{"model":"dumate-deepseek-v4-pro","stream":true,"messages":[]}`)
	prepared, errPrepare := prepareBody(body, true)
	if errPrepare != nil {
		t.Fatalf("unexpected error: %v", errPrepare)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(prepared, &payload); errUnmarshal != nil {
		t.Fatalf("prepared body is not JSON: %v", errUnmarshal)
	}
	if payload["model"] != "deepseek-v4-pro" {
		t.Fatalf("model = %v, want deepseek-v4-pro", payload["model"])
	}
	options, okOptions := payload["stream_options"].(map[string]any)
	if !okOptions || options["include_usage"] != true {
		t.Fatalf("stream_options 未补齐: %v", payload["stream_options"])
	}
}

func TestPrepareBodyKeepsClientStreamOptions(t *testing.T) {
	body := []byte(`{"model":"glm-53","stream":true,"stream_options":{"include_usage":false}}`)
	prepared, errPrepare := prepareBody(body, true)
	if errPrepare != nil {
		t.Fatalf("unexpected error: %v", errPrepare)
	}
	if !strings.Contains(string(prepared), `"include_usage":false`) {
		t.Fatalf("客户端显式给的值被覆盖: %s", prepared)
	}
}

func TestUpstreamModelStripsPrefix(t *testing.T) {
	if got := upstreamModel("glm-53"); got != "glm-53" {
		t.Fatalf("无前缀应原样返回，got %q", got)
	}
	if got := upstreamModel("dumate-glm-53"); got != "glm-53" {
		t.Fatalf("注册名应剥掉宿主前缀，got %q", got)
	}
	if got := upstreamModel("some-unknown-model"); got != "some-unknown-model" {
		t.Fatalf("未知模型应透传，got %q", got)
	}
}

// TestUpstreamModelMapsSuffixedID 锁定带 `-dm` 后缀的注册名要映射回上游原名。
//
// 宿主对模型名做归一化去重（去点、去横线后比对），`glm-53` 会被当成已有
// `glm-5.3` 的变体直接丢弃，所以这几个模型注册时带了后缀。如果这条映射
// 断了，客户端调 `dumate-glm-53-dm` 会把 `-dm` 一起发给上游，直接 404。
func TestUpstreamModelMapsSuffixedID(t *testing.T) {
	cases := map[string]string{
		"dumate-glm-53-dm":             "glm-53",
		"glm-53-dm":                    "glm-53",
		"dumate-glm-53-flash-dm":       "glm-53-flash",
		"dumate-deepseek-v41-flash-dm": "deepseek-v41-flash",
		"dumate-kimi-k31-dm":           "kimi-k31",
		"dumate-qwen38-flash-dm":       "qwen38-flash",
		"dumate-model-text":            "model-text",
		"dumate-deepseek-v4-pro":       "deepseek-v4-pro",
	}
	for input, want := range cases {
		if got := upstreamModel(input); got != want {
			t.Fatalf("upstreamModel(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestConsoleSessionDetection 锁定「什么才算能用的搭子凭证」。
//
// 这条判据来自实测：只登录百度账号页拿到的 Cookie 里没有
// bce-long-term-sessionid，调 /api/dumate/* 一律 401「登录已经过期」。
// 一旦有人把它改回「有 BDUSS 就行」，用户又会掉进同一个坑。
func TestConsoleSessionDetection(t *testing.T) {
	full := "BDUSS=abc; STOKEN=d; bce-long-term-sessionid=xyz; bce-user-info=1"
	if !HasConsoleSession(full) {
		t.Fatal("应识别出 console 域长会话")
	}
	if HasConsoleSession("BDUSS=abc; STOKEN=d") {
		t.Fatal("只有 BDUSS/STOKEN 不应判定为完整凭证")
	}
	if got := BDUSSFromCookie("BDUSS=abc; x=1"); got != "abc" {
		t.Fatalf("BDUSSFromCookie = %q, want abc", got)
	}
	if got := CookieField(full, "bce-long-term-sessionid"); got != "xyz" {
		t.Fatalf("CookieField = %q, want xyz", got)
	}
}
