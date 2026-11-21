package qoder

import (
	"net/http"
	"testing"
	"time"

	"freetier2api-plugin/internal/core"
)

// TestUpstreamErrorQueueMapsTo503 锁住排队错误的对外契约。
//
// 回归点：UpstreamError 曾只实现 Error()，ToPluginError 归类失败后一律兜底
// 502，客户端把「排队」当服务器故障立刻重打，正好加剧高峰。排队必须是
// 503 + Retry-After，且不触发宿主换号（模型级排队与账号无关）。
func TestUpstreamErrorQueueMapsTo503(t *testing.T) {
	upstream := &UpstreamError{
		Status:  http.StatusServiceUnavailable,
		ErrType: ErrTypeModelBusy,
		Message: "上游模型 auto 排队中（队列 p3）",
		Queue:   &QueueSignal{Queued: true, ModelKey: "auto", RetryAfterSeconds: 30},
	}

	var contract core.UpstreamError = upstream
	if got := contract.HTTPStatus(); got != http.StatusServiceUnavailable {
		t.Fatalf("HTTPStatus = %d, want 503", got)
	}
	if contract.Retryable() {
		t.Fatal("排队不应触发换号重试")
	}
	if got := contract.UpstreamKind(); got != core.FailureKindBusy.String() {
		t.Fatalf("UpstreamKind = %q, want %q", got, core.FailureKindBusy)
	}

	var hinter core.RetryHinter = upstream
	if got := hinter.RetryAfter(); got != 30*time.Second {
		t.Fatalf("RetryAfter = %s, want 30s", got)
	}

	pluginErr, ok := core.ToPluginError(upstream).(*core.PluginError)
	if !ok {
		t.Fatalf("ToPluginError 返回 %T，期望 *core.PluginError", core.ToPluginError(upstream))
	}
	if pluginErr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("plugin HTTPStatus = %d, want 503", pluginErr.HTTPStatus)
	}
	if pluginErr.RetryAfterSeconds != 30 {
		t.Fatalf("RetryAfterSeconds = %d, want 30", pluginErr.RetryAfterSeconds)
	}
	if pluginErr.Retryable {
		t.Fatal("plugin.Retryable 应为 false")
	}
}

// TestUpstreamErrorContentPolicyMapsTo400 内容审核是用户侧问题：400、不重试。
func TestUpstreamErrorContentPolicyMapsTo400(t *testing.T) {
	upstream := &UpstreamError{ErrType: ErrTypeContentPolicy, Message: "内容审核未通过"}
	var contract core.UpstreamError = upstream
	if got := contract.HTTPStatus(); got != http.StatusBadRequest {
		t.Fatalf("HTTPStatus = %d, want 400", got)
	}
	if contract.Retryable() {
		t.Fatal("内容审核不应重试")
	}
}

// TestUpstreamErrorTransientMapsTo502 瞬时故障仍按 502 可重试，保持既有行为。
func TestUpstreamErrorTransientMapsTo502(t *testing.T) {
	upstream := &UpstreamError{Status: http.StatusBadGateway, ErrType: ErrTypeTransient, Message: "上游抖动"}
	var contract core.UpstreamError = upstream
	if got := contract.HTTPStatus(); got != http.StatusBadGateway {
		t.Fatalf("HTTPStatus = %d, want 502", got)
	}
	if !contract.Retryable() {
		t.Fatal("瞬时故障应可重试")
	}
}
