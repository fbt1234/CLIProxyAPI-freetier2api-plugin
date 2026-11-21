package workbuddy

import (
	"testing"
	"time"

	"freetier2api-plugin/internal/core"
)

// TestNonChatModelFiltersVideoFamilies 锁住视频/音频模型过滤。
//
// 回归点：上游目录把 kling-video-v3、seedance-2.5 当普通模型下发，但调用
// 必报 400 "Backend [maas] is not supported"；列表里出现它们会让客户端选到
// 必失败的模型。
func TestNonChatModelFiltersVideoFamilies(t *testing.T) {
	cases := []struct {
		name  string
		entry dynModelEntry
		want  bool
	}{
		{"kling 视频", dynModelEntry{ID: "kling-video-v3"}, true},
		{"seedance 视频", dynModelEntry{ID: "seedance-2.5"}, true},
		{"sora 视频", dynModelEntry{ID: "sora-2"}, true},
		{"veo 视频", dynModelEntry{ID: "veo-3.1"}, true},
		{"视频标签", dynModelEntry{ID: "mystery-model", Tags: []string{"text-to-video"}}, true},
		{"音频标签", dynModelEntry{ID: "mystery-voice", Tags: []string{"Text-To-Speech"}}, true},
		{"文生图标签", dynModelEntry{ID: "mystery-image", Tags: []string{"text-to-image"}}, true},
		{"普通对话模型", dynModelEntry{ID: "deepseek-v4-flash", MaxOutputTokens: 50000}, false},
		{"带视频理解能力的对话模型", dynModelEntry{ID: "gemini-3.5-pro", Tags: []string{"vision"}, MaxOutputTokens: 64000}, false},
	}
	for _, tc := range cases {
		if got := nonChatModel(tc.entry); got != tc.want {
			t.Fatalf("%s: nonChatModel = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFilterBlockedModels 锁住 11102 隔离：观察到的坏模型从清单里摘掉，
// 另一个域不受影响，过期后自动回归，全被隔离时不清空清单。
func TestFilterBlockedModels(t *testing.T) {
	rt := newRuntime(core.Env{})
	models := []ModelInfo{{ID: "o4-mini"}, {ID: "deepseek-v4-flash"}}

	rt.markModelBlocked(RegionGlobal, "o4-mini")

	global := rt.filterBlockedModels(RegionGlobal, models)
	if len(global) != 1 || global[0].ID != "deepseek-v4-flash" {
		t.Fatalf("global 过滤后 = %#v", global)
	}
	if cn := rt.filterBlockedModels(RegionCN, models); len(cn) != 2 {
		t.Fatalf("cn 不应被 global 的隔离影响，得到 %#v", cn)
	}

	// 过期后自动放行。
	rt.modelMu.Lock()
	rt.blockedModels[blockedKey(RegionGlobal, "o4-mini")] = time.Now().Add(-blockedModelTTL - time.Minute)
	rt.modelMu.Unlock()
	if got := rt.filterBlockedModels(RegionGlobal, models); len(got) != 2 {
		t.Fatalf("过期后应全部回归，得到 %#v", got)
	}

	// 全部被隔离时不清空清单。
	rt.markModelBlocked(RegionGlobal, "o4-mini")
	rt.markModelBlocked(RegionGlobal, "deepseek-v4-flash")
	if got := rt.filterBlockedModels(RegionGlobal, models); len(got) != 2 {
		t.Fatalf("全隔离时应原样返回，得到 %#v", got)
	}
}
