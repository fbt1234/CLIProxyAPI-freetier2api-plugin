package workbuddy

// 流式转发的性能基准与归一化回归。
//
// 每帧的路径是「解帧 → 白名单重建 → 序列化 → 交给宿主」，一次长回复有几百帧，
// 这段开销全压在转发链上。基准锁住「别退回 map 往返」这条线（map 路线每帧
// 约 80 次分配、4.7KB 垃圾）。

import (
	"bytes"
	"strings"
	"testing"
)

// benchStreamBody 造一段含 n 个纯文本增量帧的上游 SSE。
func benchStreamBody(n int) []byte {
	var builder strings.Builder
	for i := 0; i < n; i++ {
		builder.WriteString(`data: {"id":"chatcmpl-9f2c1e","object":"chat.completion.chunk","created":1763300000,"model":"glm-5.2","choices":[{"index":0,"delta":{"content":"性能测试的一段输出文本"},"finish_reason":null}],"usage":null}`)
		builder.WriteString("\n\n")
	}
	builder.WriteString("data: [DONE]\n\n")
	return []byte(builder.String())
}

// BenchmarkStreamTextFrames 是纯文本增量帧的转发开销。
func BenchmarkStreamTextFrames(b *testing.B) {
	body := benchStreamBody(500)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		stats, errStream := Stream(bytes.NewReader(body), StreamOptions{
			Emit: func([]byte) error { return nil },
		})
		if errStream != nil {
			b.Fatal(errStream)
		}
		if stats.Frames != 500 {
			b.Fatalf("frames = %d, want 500", stats.Frames)
		}
	}
}

// TestStreamToolCallNamesOnlyOnFirstChunk 验证工具调用名的首片语义。
func TestStreamToolCallNamesOnlyOnFirstChunk(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}]}}]}`,
		"",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var frames []string
	if _, errStream := Stream(strings.NewReader(stream), StreamOptions{
		Emit: func(payload []byte) error {
			frames = append(frames, string(payload))
			return nil
		},
	}); errStream != nil {
		t.Fatalf("Stream: %v", errStream)
	}
	if len(frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(frames))
	}
	if !strings.Contains(frames[0], `"name":"Bash"`) {
		t.Fatalf("first chunk must keep function.name: %s", frames[0])
	}
	if strings.Contains(frames[1], `"name"`) {
		t.Fatalf("later chunks must drop function.name: %s", frames[1])
	}
}

// TestStreamFillsMissingID 上游漏 id 时用首帧 id 续传。
func TestStreamFillsMissingID(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-a","choices":[{"index":0,"delta":{"content":"a"}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"b"}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var frames []string
	if _, errStream := Stream(strings.NewReader(stream), StreamOptions{
		Emit: func(payload []byte) error {
			frames = append(frames, string(payload))
			return nil
		},
	}); errStream != nil {
		t.Fatalf("Stream: %v", errStream)
	}
	if len(frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(frames))
	}
	if !strings.Contains(frames[1], `"id":"chatcmpl-a"`) {
		t.Fatalf("missing id must be filled from first frame: %s", frames[1])
	}
}
