package dumate

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDecodeJSONP(t *testing.T) {
	cases := map[string]string{
		`cb({"errno":0,"sign":"abc"})`:           "abc",
		`({"errno":0,"sign":"xyz"})`:             "xyz",
		`{"errno":0,"sign":"raw"}`:               "raw",
		"cb({\"errno\":0,\"sign\":\"spaced\"} )": "spaced",
	}
	for input, want := range cases {
		payload, errDecode := decodeJSONP([]byte(input))
		if errDecode != nil {
			t.Fatalf("%s => 解析失败: %v", input, errDecode)
		}
		if got, _ := payload["sign"].(string); got != want {
			t.Fatalf("%s => sign=%q, want %q", input, got, want)
		}
	}
	if _, errBad := decodeJSONP([]byte("not json")); errBad == nil {
		t.Fatal("非 JSON 输入应报错")
	}
}

func TestIsTimeout(t *testing.T) {
	if !isTimeout(context.DeadlineExceeded) {
		t.Fatal("deadline exceeded 应判定为超时")
	}
	if !isTimeout(errString("net/http: request canceled (Client.Timeout exceeded)")) {
		t.Fatal("Timeout 文本应判定为超时")
	}
	if isTimeout(nil) {
		t.Fatal("nil 不是超时")
	}
	if isTimeout(errString("connection refused")) {
		t.Fatal("连接拒绝不是超时")
	}
}

func TestRandomUUIDFormat(t *testing.T) {
	uuid := randomUUID()
	parts := strings.Split(uuid, "-")
	if len(parts) != 5 {
		t.Fatalf("UUID 段数=%d, want 5 (%s)", len(parts), uuid)
	}
	for index, length := range []int{8, 4, 4, 4, 12} {
		if len(parts[index]) != length {
			t.Fatalf("第 %d 段长度=%d, want %d (%s)", index+1, len(parts[index]), length, uuid)
		}
	}
	if parts[2][0] != '4' {
		t.Fatalf("version 位应为 4：%s", uuid)
	}
}

func TestLoginChannelParse(t *testing.T) {
	// 扫码未确认：status=0 且 v 为空 → 只是「已扫」，不该兑换。
	payload, errDecode := decodeJSONP([]byte(`({"errno":0,"channel_v":"{\"status\":0,\"v\":\"\"}"})`))
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	var inner struct {
		Status any    `json:"status"`
		V      string `json:"v"`
	}
	raw, _ := payload["channel_v"].(string)
	if errJSON := json.Unmarshal([]byte(raw), &inner); errJSON != nil {
		t.Fatal(errJSON)
	}
	if inner.V != "" {
		t.Fatalf("未确认时 v 应为空，got %q", inner.V)
	}

	// 已确认：v 是临时 bduss。
	payload, _ = decodeJSONP([]byte(`({"errno":0,"channel_v":"{\"status\":1,\"v\":\"abcdefghijklmnopqrstuvwxyz0123456789\"}"})`))
	raw, _ = payload["channel_v"].(string)
	inner = struct {
		Status any    `json:"status"`
		V      string `json:"v"`
	}{}
	if errJSON := json.Unmarshal([]byte(raw), &inner); errJSON != nil {
		t.Fatal(errJSON)
	}
	if len(inner.V) < 20 {
		t.Fatalf("确认后应带临时 bduss，got %q", inner.V)
	}
}

func TestPassportHeaders(t *testing.T) {
	request, errRequest := http.NewRequest(http.MethodGet, passportBase, nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	applyPassportHeaders(request)
	if !strings.Contains(request.Header.Get("User-Agent"), "Chrome") {
		t.Fatalf("UA 必须是桌面浏览器：%q", request.Header.Get("User-Agent"))
	}
}

type errString string

func (e errString) Error() string { return string(e) }
