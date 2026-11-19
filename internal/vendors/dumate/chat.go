package dumate

// 本文件实现搭子的 chat-completions 转发。
//
// 云端网关是标准 OpenAI 协议，请求体基本不用改写，只有两处必须动：
//   - model：注册名（dumate-deepseekv4-pro）要换成上游路由名（deepseek-v4-pro）；
//   - stream_options.include_usage：云端不默认回 usage 帧，漏了它客户端拿不到
//     用量（桌面链路默认给，这是网页链路唯一的协议差异）。
//
// 401 要重试一次：token 可能在本地过期时间之前就被服务端作废（同账号在别处
// 重新登录会顶掉旧票）。只重试一次——真失效（Cookie 过期）时重试多少次都没用。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// Chat 执行一次非流式对话，返回上游完整响应体。
func Chat(ctx context.Context, gatewayHost, token string, body []byte) ([]byte, error) {
	payload, errPrepare := prepareBody(body, false)
	if errPrepare != nil {
		return nil, errPrepare
	}
	req, errReq := buildChatRequest(ctx, gatewayHost, token, payload, false)
	if errReq != nil {
		return nil, errReq
	}
	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "dumate chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(raw))
	}
	return raw, nil
}

// ChatStream 执行一次流式对话，返回上游响应体（调用方负责关闭）。
func ChatStream(ctx context.Context, gatewayHost, token string, body []byte) (*http.Response, error) {
	payload, errPrepare := prepareBody(body, true)
	if errPrepare != nil {
		return nil, errPrepare
	}
	req, errReq := buildChatRequest(ctx, gatewayHost, token, payload, true)
	if errReq != nil {
		return nil, errReq
	}
	// 超时传 0：流式总时长不可预期，一次性超时会把长回答截断。
	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "dumate chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, string(raw))
	}
	return resp, nil
}

// buildChatRequest 组装一次网关请求。
func buildChatRequest(ctx context.Context, gatewayHost, token string, payload []byte, stream bool) (*http.Request, error) {
	endpoint := GatewayHost(gatewayHost) + GatewayChatPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	gatewayHeaders(req, token, stream)
	return req, nil
}

// prepareBody 改写模型名并补齐 usage 帧开关。
//
// 解析失败时原样转发：上游会给出比插件更准确的错误（"model not found"），
// 自己抛出 "invalid json" 反而是噪声。
func prepareBody(raw []byte, stream bool) ([]byte, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return raw, nil
	}
	changed := false
	if model, okModel := payload["model"].(string); okModel && model != "" {
		if mapped := upstreamModel(model); mapped != model {
			payload["model"] = mapped
			changed = true
		}
	}
	// 流式：云端默认不回 usage，必须显式要；客户端已经给了值就不覆盖
	// （有些客户端显式关掉它，是为了省最后一帧）。
	if stream {
		if _, okUsage := payload["stream_options"]; !okUsage {
			payload["stream_options"] = map[string]any{"include_usage": true}
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return raw, nil
	}
	return encoded, nil
}
