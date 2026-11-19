package dumate

// 本文件实现搭子的上游错误分类。
//
// 分类决定宿主怎么处置账号：
//   - 401/403 → 凭证失效（Cookie 过期），让宿主标记坏号并停止路由；
//   - 402 → 积分耗尽（搭子是积分制，用完了要等签到）；
//   - 429 → 限流，保留账号稍后重试；
//   - 5xx/网络 → 上游抖动，保留账号；
//   - 4xx → 这次请求本身的问题（模型名不存在、Prompt 超长），账号没问题。

import (
	"encoding/json"
	"net/http"
	"strings"

	"freetier2api-plugin/internal/core"
)

// Classify 按 HTTP 状态码分类一个上游错误。
func Classify(status int, body string) *core.Failure {
	msg := core.Truncate(extractMessage(body), 300)
	if msg == "" {
		msg = core.Truncate(body, 300)
	}
	if status <= 0 {
		status = http.StatusBadGateway
	}
	return &core.Failure{
		Status: status,
		Msg:    msg,
		Kind:   core.ClassifyStatus(status),
	}
}

// extractMessage 从上游错误体里抠出可读说明。
//
// 搭子的网关错误体是 OpenAI 形状（{"error":{"message":...}}），网页端是
// {"message":...} / {"msg":...}；把两种都试一遍，用户看到的才是真实原因
// 而不是一段裸 JSON。
func extractMessage(body string) string {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal([]byte(strings.TrimSpace(body)), &payload); errUnmarshal != nil {
		return ""
	}
	if nested, okNested := payload["error"].(map[string]any); okNested {
		if message, okMessage := nested["message"].(string); okMessage && message != "" {
			return message
		}
	}
	for _, key := range []string{"message", "msg", "error_message", "error"} {
		if message, okMessage := payload[key].(string); okMessage && message != "" {
			return message
		}
	}
	return ""
}
