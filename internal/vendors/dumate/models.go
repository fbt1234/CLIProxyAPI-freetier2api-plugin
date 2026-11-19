package dumate

// 本文件实现搭子的模型清单。
//
// 网页凭证走的是云端网关 `dumate-svc.baidu.com/gateway/apis/v1`，它只认
// **七个路由名**（2026-11-06 真机实测，个人版与企业租户同一套）：
//
//	model-text          glm-53              deepseek-v4-pro
//	kimi-k31            glm-53-flash        deepseek-v41-flash
//	qwen38-flash
//
// 社区流传的其它名字（deepseek-v4.1 / kimi-k3.1-pro / glm-5.3 / qwen-3.8-flash）
// 与桌面后端真名（deepseek-v4.1-flash-low-latency / glm-5.2）打到云端网关
// 一律 404。桌面链路（本机 8980）才有 dm-auto-model/*、doubao、ernie 那批
// 模型，本插件不走桌面链路，因此**不注册**它们——注册了只会让客户端看到
// 一批必然失败的模型。
//
// 清单是固定的（上游没有公开的模型目录接口），不做网络探测：探测失败会让
// 整家供应商从 /v1/models 上消失，而这个清单本身不会因为探测而变多。

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

// Model 是一个搭子模型。
type Model struct {
	// ID 是注册到 CPA 的模型名（客户端请求用的名字）。
	ID string
	// Upstream 是打上游网关用的路由名。
	Upstream string
	// Name 是展示名。
	Name string
	// Description 是说明。
	Description string
	// ContextWindow 是上下文窗口。
	ContextWindow int64
	// MaxOutputTokens 是输出上限。
	MaxOutputTokens int64
	// SupportsImages 报告是否接受图片输入。
	SupportsImages bool
	// Thinking 报告是否支持思考档位（搭子网关不透出档位，一律 false）。
	Thinking bool
}

// upstreamDeepSeekPro 是 DeepSeek V4 Pro 的上游路由名。
const upstreamDeepSeekPro = "deepseek-v4-pro"

// 注册名一般就是上游路由名：宿主注册时会统一加 `<vendorID>-` 前缀
// （dumate-model-text），因此这里**不要**自己再拼一遍，否则会变成
// dumate-dumate-model-text。
//
// 例外是带 `-dm` 后缀的那五个。宿主在注册前会把模型名归一化（去掉点与
// 横线）再与已有模型比对，撞上就当作「别家模型的变体」**静默丢弃**：
// `glm-53` 撞 `glm-5.3`、`kimi-k31` 撞 `kimi-k3.1`、`qwen38-flash` 撞
// `qwen3.8-flash`、`deepseek-v41-flash` 撞 `deepseek-v4.1-flash`。
// 表现是插件明明交了 7 个模型，/v1/models 上只剩 2 个。加后缀是为了让归一化
// 后的名字不再与任何已有模型相同；真实路由靠 Model.Upstream 映射回原名。
func bundledModels() []Model {
	return []Model{
		{
			ID: "model-text", Upstream: "model-text",
			Name: "DuMate 智能路由", Description: "搭子默认智能路由，按任务自动选后端（实测落在 deepseek-v4.1-flash）",
			ContextWindow: 1000000, MaxOutputTokens: 384000, SupportsImages: true,
		},
		{
			ID: upstreamDeepSeekPro, Upstream: upstreamDeepSeekPro,
			Name: "DeepSeek V4 Pro (搭子)", Description: "搭子通道的 DeepSeek V4 Pro",
			ContextWindow: 1000000, MaxOutputTokens: 384000,
		},
		{
			ID: "deepseek-v41-flash-dm", Upstream: "deepseek-v41-flash",
			Name: "DeepSeek V4.1 Flash (搭子)", Description: "低延迟主力档，支持图片输入",
			ContextWindow: 1000000, MaxOutputTokens: 384000, SupportsImages: true,
		},
		{
			ID: "kimi-k31-dm", Upstream: "kimi-k31",
			Name: "Kimi K3.1 (搭子)", Description: "长上下文，支持图片与视频输入",
			ContextWindow: 1048576, MaxOutputTokens: 1048576, SupportsImages: true,
		},
		{
			ID: "glm-53-dm", Upstream: "glm-53",
			Name: "GLM-5.3 (搭子)", Description: "智谱 GLM-5.3",
			ContextWindow: 1000000, MaxOutputTokens: 128000,
		},
		{
			ID: "glm-53-flash-dm", Upstream: "glm-53-flash",
			Name: "GLM-5.3 Flash (搭子)", Description: "智谱 GLM-5.3 快速档，支持图片与视频输入",
			ContextWindow: 1000000, MaxOutputTokens: 128000, SupportsImages: true,
		},
		{
			ID: "qwen38-flash-dm", Upstream: "qwen38-flash",
			Name: "Qwen3.8 Flash (搭子)", Description: "通义千问快速档，支持图片与视频输入",
			ContextWindow: 983000, MaxOutputTokens: 128000, SupportsImages: true,
		},
	}
}

// StaticModels 返回本供应商的全部模型。
//
// 不探测上游：清单是固定的，探测只会引入「上游抖动 → 整家消失」的故障面。
func (v *adapter) StaticModels(_ context.Context) ([]pluginapi.ModelInfo, error) {
	models := bundledModels()
	// 留痕：宿主有时会把插件给出的模型「静默丢弃」（与已有模型名归一后重名），
	// 一旦 /v1/models 上少了条目，只能靠这行日志判断是插件少给了还是宿主没接。
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	logger.Info("dumate：StaticModels 交出 %d 个模型 %v", len(models), ids)
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI 把模型条目转成宿主契约类型。
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:              id,
			Name:            core.FirstNonEmpty(model.Name, id),
			Description:     strings.TrimSpace(model.Description),
			ContextLength:   model.ContextWindow,
			MaxOutputTokens: model.MaxOutputTokens,
			SupportsTools:   true,
		}, true
	})
}

// upstreamModel 把客户端传来的注册名映射回上游路由名。
//
// 两步：
//  1. 先按 bundledModels 的 ID 精确查表，拿 Upstream。带 `-dm` 后缀的那几个
//     必须靠这一步：宿主对模型名做归一化去重（去点、去横线后比对），
//     `glm-53` 会被判成已有模型 `glm-5.3` 的变体并**静默丢弃**，
//     所以注册名要加后缀区分，而上游只认原名。
//  2. 查不到再剥宿主加的 `dumate-` 前缀，让网关自己判定存不存在——
//     比插件硬编码白名单更宽容。
func upstreamModel(registered string) string {
	trimmed := strings.TrimSpace(registered)
	if trimmed == "" {
		return ""
	}
	if upstream := lookupUpstream(trimmed); upstream != "" {
		return upstream
	}
	if len(trimmed) > len(VendorID)+1 && strings.EqualFold(trimmed[:len(VendorID)+1], VendorID+"-") {
		stripped := trimmed[len(VendorID)+1:]
		if upstream := lookupUpstream(stripped); upstream != "" {
			return upstream
		}
		return stripped
	}
	return trimmed
}

// lookupUpstream 在清单里按注册名找上游路由名。
func lookupUpstream(registered string) string {
	for _, model := range bundledModels() {
		if strings.EqualFold(strings.TrimSpace(model.ID), registered) {
			return model.Upstream
		}
	}
	return ""
}
