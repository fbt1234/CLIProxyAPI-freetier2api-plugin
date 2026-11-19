//go:build live

package workbuddy

// 流式转发实况探针：用真实账号跑一次 ExecuteStream，打印帧数与首尾帧，
// 确认归一化后的帧形态对真实上游成立（只打印帧内容，不含凭证）。
//
// 这里自带一个支持 do_stream / stream_read / stream_close 的假宿主桥：
// 真正的宿主流式能力由 CLIProxyAPI 提供，探针里用真实 HTTP 流模拟。
//
//	WORKBUDDY_LIVE=1 WORKBUDDY_LIVE_CREDS=<auth.json> WORKBUDDY_LIVE_REGION=global \
//	  go test -tags live ./internal/vendors/workbuddy/ -run TestLiveStreamFrames -v

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

type probeStream struct {
	body io.ReadCloser
}

var (
	probeStreamsMu sync.Mutex
	probeStreams   = map[string]*probeStream{}
	probeStreamSeq atomic.Int64
)

// configureWorkBuddyStreamProbeHost 让 hostbridge 直接打真实上游，含流式读取。
func configureWorkBuddyStreamProbeHost() {
	client := &http.Client{}
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		rawPayload, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var request struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		switch method {
		case pluginabi.MethodHostHTTPDo, pluginabi.MethodHostHTTPDoStream:
			if errDecode := json.Unmarshal(rawPayload, &request); errDecode != nil {
				return nil, errDecode
			}
			var bodyReader io.Reader
			if len(request.Body) > 0 {
				bodyReader = strings.NewReader(string(request.Body))
			}
			httpRequest, errNew := http.NewRequest(request.Method, request.URL, bodyReader)
			if errNew != nil {
				return nil, errNew
			}
			for key, values := range request.Headers {
				for _, value := range values {
					httpRequest.Header.Add(key, value)
				}
			}
			response, errDo := client.Do(httpRequest)
			if errDo != nil {
				return nil, errDo
			}
			if method == pluginabi.MethodHostHTTPDo {
				defer func() { _ = response.Body.Close() }()
				responseBody, errRead := io.ReadAll(response.Body)
				if errRead != nil {
					return nil, errRead
				}
				return json.Marshal(map[string]any{
					"StatusCode": response.StatusCode, "Headers": response.Header, "Body": responseBody,
				})
			}
			streamID := fmt.Sprintf("probe-stream-%d", probeStreamSeq.Add(1))
			probeStreamsMu.Lock()
			probeStreams[streamID] = &probeStream{body: response.Body}
			probeStreamsMu.Unlock()
			return json.Marshal(map[string]any{
				"status_code": response.StatusCode, "headers": response.Header, "stream_id": streamID,
			})
		case pluginabi.MethodHostHTTPStreamRead:
			var readRequest struct {
				StreamID string `json:"stream_id"`
			}
			if errDecode := json.Unmarshal(rawPayload, &readRequest); errDecode != nil {
				return nil, errDecode
			}
			probeStreamsMu.Lock()
			stream := probeStreams[readRequest.StreamID]
			probeStreamsMu.Unlock()
			if stream == nil {
				return nil, fmt.Errorf("unknown stream %s", readRequest.StreamID)
			}
			buffer := make([]byte, 4096)
			n, errRead := stream.body.Read(buffer)
			if n > 0 {
				return json.Marshal(map[string]any{"payload": buffer[:n]})
			}
			if errRead == io.EOF {
				return json.Marshal(map[string]any{"done": true})
			}
			if errRead != nil {
				return json.Marshal(map[string]any{"error": errRead.Error()})
			}
			return json.Marshal(map[string]any{})
		case pluginabi.MethodHostHTTPStreamClose:
			var closeRequest struct {
				StreamID string `json:"stream_id"`
			}
			if errDecode := json.Unmarshal(rawPayload, &closeRequest); errDecode != nil {
				return nil, errDecode
			}
			probeStreamsMu.Lock()
			stream := probeStreams[closeRequest.StreamID]
			delete(probeStreams, closeRequest.StreamID)
			probeStreamsMu.Unlock()
			if stream != nil {
				_ = stream.body.Close()
			}
			return json.Marshal(map[string]any{})
		}
		return nil, fmt.Errorf("probe host: unsupported method %s", method)
	})
}

// probeSink 收集分片。
type probeSink struct {
	onFrame func([]byte)
}

func (s *probeSink) Emit(payload []byte) error {
	s.onFrame(payload)
	return nil
}

func (s *probeSink) Close(string) error { return nil }

func TestLiveStreamFrames(t *testing.T) {
	if os.Getenv("WORKBUDDY_LIVE") == "" {
		t.Skip("WORKBUDDY_LIVE 未设置")
	}
	path := os.Getenv("WORKBUDDY_LIVE_CREDS")
	if path == "" {
		t.Skip("WORKBUDDY_LIVE_CREDS 未设置")
	}
	region := RegionGlobal
	if os.Getenv("WORKBUDDY_LIVE_REGION") == "cn" {
		region = RegionCN
	}
	model := os.Getenv("WORKBUDDY_LIVE_MODEL")
	if model == "" {
		model = "deepseek-v4.1-flash"
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read creds: %v", errRead)
	}
	configureWorkBuddyStreamProbeHost()

	inst := newInstance(core.Env{}, region)
	cred, errParse := inst.Parse(raw, path)
	if errParse != nil {
		t.Fatalf("parse creds: %v", errParse)
	}

	payload := []byte(`{"model":"` + model + `","stream":true,"seed":1234567890123456789,"messages":[{"role":"user","content":"用一句话介绍你自己"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var frames []string
	errStream := inst.ExecuteStream(ctx, cred, &core.ExecuteRequest{
		Model:   model,
		Payload: payload,
		Stream:  true,
	}, &probeSink{onFrame: func(chunk []byte) {
		frames = append(frames, string(chunk))
	}})
	if errStream != nil {
		t.Fatalf("ExecuteStream: %v", errStream)
	}
	if len(frames) == 0 {
		t.Fatal("no frames emitted")
	}
	t.Logf("帧数=%d", len(frames))
	t.Logf("首帧=%s", frames[0])
	if len(frames) > 1 {
		t.Logf("次帧=%s", frames[1])
	}
	t.Logf("末帧=%s", frames[len(frames)-1])
	// 拼接所有 content，确认文本没被改写坏。
	var text strings.Builder
	for _, frame := range frames {
		var parsed struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if errUnmarshal := json.Unmarshal([]byte(frame), &parsed); errUnmarshal != nil {
			// 非 data: 行（event: 等）原样透传，不是 JSON。
			if strings.HasPrefix(frame, "{") {
				t.Fatalf("frame is not valid JSON: %v (%s)", errUnmarshal, frame)
			}
			continue
		}
		for _, choice := range parsed.Choices {
			text.WriteString(choice.Delta.Content)
		}
	}
	t.Logf("拼出的正文=%q", text.String())
}

// TestLiveChatNonStream 跑一次非流式对话，确认 UseNumber 改造后请求体与聚合仍然正常。
func TestLiveChatNonStream(t *testing.T) {
	if os.Getenv("WORKBUDDY_LIVE") == "" {
		t.Skip("WORKBUDDY_LIVE 未设置")
	}
	path := os.Getenv("WORKBUDDY_LIVE_CREDS")
	if path == "" {
		t.Skip("WORKBUDDY_LIVE_CREDS 未设置")
	}
	region := RegionGlobal
	if os.Getenv("WORKBUDDY_LIVE_REGION") == "cn" {
		region = RegionCN
	}
	model := os.Getenv("WORKBUDDY_LIVE_MODEL")
	if model == "" {
		model = "deepseek-v4.1-flash"
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read creds: %v", errRead)
	}
	configureWorkBuddyStreamProbeHost()
	inst := newInstance(core.Env{}, region)
	cred, errParse := inst.Parse(raw, path)
	if errParse != nil {
		t.Fatalf("parse creds: %v", errParse)
	}
	payload := []byte(`{"model":"` + model + `","stream":false,"max_completion_tokens":512,"seed":1234567890123456789,"messages":[{"role":"user","content":"只回四个字：收到请回复"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	response, errExecute := inst.Execute(ctx, cred, &core.ExecuteRequest{Model: model, Payload: payload})
	if errExecute != nil {
		t.Fatalf("Execute: %v", errExecute)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(response.Payload, &parsed); errUnmarshal != nil {
		t.Fatalf("response is not valid JSON: %v (%s)", errUnmarshal, response.Payload)
	}
	if len(parsed.Choices) == 0 {
		t.Fatalf("no choices in response: %s", response.Payload)
	}
	t.Logf("正文=%q usage=%v", parsed.Choices[0].Message.Content, parsed.Usage["total_tokens"])
}
