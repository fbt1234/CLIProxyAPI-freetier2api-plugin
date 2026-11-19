package dumate

// 本文件实现搭子的签到与积分查询。
//
// 签到（网页端，Cookie 认证）：
//
//	GET  /api/dumate/points/loginBonusInfo   → { hasIssued, totalPoints, signInDays }
//	POST /api/dumate/points/loginBonus       → 领取当日积分（每日 500 分）
//
// 上游按天幂等：已领过再 POST 不会重复发放，但仍会回一次成功。因此签到前
// 先查状态，已领过就如实报「今日已签」——不要靠 POST 的结果反推，否则
// 任务页会把重复签到记成一次新的成功。
//
// 积分总览：
//
//	GET /api/dumate/points/quota_overview?clientType=desktop&timezone=Asia/Shanghai
//	    → { totalPoints, usedPoints, subscription[], incremental[] }
//
// 注意：totalPoints 可能把**其它百度产品**的包也算进来（实测有账号带
// product:"comate" 的 5000 分），所以这里只做展示，不用它做额度判断。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// SupportsCheckin 报告本供应商提供签到。
func (v *adapter) SupportsCheckin() bool { return true }

// Checkin 执行一次签到。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	info, errInfo := fetchLoginBonusInfo(ctx, v.webBase, native.CookieValue())
	if errInfo != nil {
		return nil, errInfo
	}
	if info.hasIssued {
		return &core.CheckinResult{
			Already: true,
			Streak:  info.totalTimes,
			Message: "今日已签到",
		}, nil
	}
	points, errClaim := claimLoginBonus(ctx, v.webBase, native.CookieValue())
	if errClaim != nil {
		return nil, errClaim
	}
	credit := points
	if credit == 0 {
		// 上游没回本次发放数时按签到档位兜底：如实报 0 会让页面显示
		// 「签到成功 +0 分」，与实际不符。
		credit = 500
	}
	return &core.CheckinResult{
		Credit:  credit,
		Streak:  info.totalTimes + 1,
		Message: "签到成功",
	}, nil
}

// loginBonusInfo 是签到状态。
type loginBonusInfo struct {
	hasIssued  bool
	totalTimes int64
	monthPoint int64
}

// fetchLoginBonusInfo 查询当日签到状态。
func fetchLoginBonusInfo(ctx context.Context, webBase, cookie string) (*loginBonusInfo, error) {
	status, payload, errDo := webRequest(ctx, webBase, http.MethodGet, LoginBonusInfoPath, cookie, nil, 20*time.Second)
	if errDo != nil {
		return nil, errDo
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, core.NewPluginError("credential_invalid", "搭子 Cookie 已失效，请重新登录", http.StatusUnauthorized)
	}
	if status < 200 || status >= 300 {
		return nil, Classify(status, string(payload))
	}
	var envelope map[string]any
	_ = json.Unmarshal(payload, &envelope)
	result := pickResult(envelope)
	info := &loginBonusInfo{
		hasIssued:  firstBool(result, "hasIssued", "has_issued"),
		totalTimes: firstInt(result, "totalTimes", "total_times"),
		monthPoint: firstInt(result, "monthPoints", "month_points"),
	}
	return info, nil
}

// claimLoginBonus 领取当日签到积分，返回本次发放的积分数（未知时为 0）。
func claimLoginBonus(ctx context.Context, webBase, cookie string) (int64, error) {
	status, payload, errDo := webRequest(ctx, webBase, http.MethodPost, LoginBonusPath, cookie, []byte("{}"), 25*time.Second)
	if errDo != nil {
		return 0, errDo
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return 0, core.NewPluginError("credential_invalid", "搭子 Cookie 已失效，请重新登录", http.StatusUnauthorized)
	}
	if status < 200 || status >= 300 {
		return 0, Classify(status, string(payload))
	}
	var envelope map[string]any
	_ = json.Unmarshal(payload, &envelope)
	if success, okSuccess := envelope["success"].(bool); okSuccess && !success {
		message := firstString(envelope, "message", "msg")
		if message == "" {
			message = "签到被上游拒绝"
		}
		return 0, core.NewPluginError("checkin_failed", message, http.StatusBadGateway)
	}
	result := pickResult(envelope)
	return firstInt(result, "points", "rewardPoints", "reward_points", "credit"), nil
}

// Quota 查询账号积分。
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	path := QuotaPath + "?clientType=desktop&timezone=Asia%2FShanghai"
	status, payload, errDo := webRequest(ctx, v.webBase, http.MethodGet, path, native.CookieValue(), nil, 20*time.Second)
	if errDo != nil {
		return nil, errDo
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, core.NewPluginError("credential_invalid", "搭子 Cookie 已失效，请重新登录", http.StatusUnauthorized)
	}
	if status < 200 || status >= 300 {
		return nil, Classify(status, string(payload))
	}
	var envelope map[string]any
	_ = json.Unmarshal(payload, &envelope)
	result := pickResult(envelope)

	total, hasTotal := firstNumber(result, "totalPoints", "total_points")
	used, hasUsed := firstNumber(result, "usedPoints", "used_points")

	resp := &pluginapi.QuotaFetchResponse{}
	if subscribed, okSub := result["isSubscribed"].(bool); okSub && subscribed {
		resp.Subscription = &pluginapi.QuotaSubscription{TierName: "搭子会员"}
	}
	// 余额不可知时**不给 0**：0 会被页面读成「积分用完」，与「上游没给字段」
	// 是两件事（上游偶发少字段是真实存在的）。
	left := 0.0
	hasLeft := hasTotal && hasUsed
	if hasLeft {
		left = total - used
		if left < 0 {
			left = 0
		}
	}
	if hasLeft {
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key: "left", Label: "剩余积分", Value: left, Unit: "积分", Format: "number",
		})
	}
	if hasTotal {
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key: "total", Label: "总积分", Value: total, Unit: "积分", Format: "number",
		})
	}
	if hasUsed {
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key: "used", Label: "已用积分", Value: used, Unit: "积分", Format: "number",
		})
	}
	return resp, nil
}

// pickResult 取业务结果体：上游有的包在 result，有的包在 data，有的直接顶层。
func pickResult(envelope map[string]any) map[string]any {
	if envelope == nil {
		return map[string]any{}
	}
	if nested, okNested := envelope["result"].(map[string]any); okNested {
		return nested
	}
	if nested, okData := envelope["data"].(map[string]any); okData {
		return nested
	}
	return envelope
}

// firstBool 返回第一个布尔字段。
func firstBool(payload map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, okValue := payload[key].(bool); okValue {
			return value
		}
	}
	return false
}

// firstInt 返回第一个整数（兼容 float64/json.Number/字符串）。
func firstInt(payload map[string]any, keys ...string) int64 {
	value, okValue := firstNumber(payload, keys...)
	if !okValue {
		return 0
	}
	return int64(value)
}

// firstNumber 返回第一个数值字段及「是否真的存在」。
func firstNumber(payload map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		switch typed := payload[key].(type) {
		case float64:
			return typed, true
		case int64:
			return float64(typed), true
		case json.Number:
			if parsed, errParse := typed.Float64(); errParse == nil {
				return parsed, true
			}
		case string:
			trimmed := strings.TrimSpace(typed)
			if trimmed == "" {
				continue
			}
			if parsed, errParse := json.Number(trimmed).Float64(); errParse == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}
