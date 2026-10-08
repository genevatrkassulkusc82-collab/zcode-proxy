package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ---- 活动检测 / 领取 / 套餐激活 ----
// 移植 zcode-switch claim.rs：
//   检测: GET  zcode.z.ai/api/v1/zcode-plan/billing/preview?app_version&platform
//   领取: POST zcode.z.ai/api/v1/zcode-plan/billing/claim {"plan_id"} + 阿里云验证码头
//   激活: POST zcode.z.ai/api/v1/event/report（app_launch + app_daily_active 两条事件）

// GrantItem 活动赠送额度明细（官方字段含 capabilities / effective_at / 单项 priority）
type GrantItem struct {
	Name         string   `json:"name"`
	Units        float64  `json:"units"`
	Period       string   `json:"period"` // one_time | daily | weekly | monthly
	Priority     int      `json:"priority,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	EffectiveAt  int64    `json:"effective_at,omitempty"` // epoch 秒（官方 effective_at）
}

// ActivityPlan 可领取活动
type ActivityPlan struct {
	PlanID      string      `json:"plan_id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Priority    int         `json:"priority"`
	Grants      []string    `json:"grants"`
	GrantItems  []GrantItem `json:"grant_items"`
}

// ClaimedPlan 领取成功后服务端返回的套餐详情（官方 data.plan）
type ClaimedPlan struct {
	UserPlanID string `json:"user_plan_id"`
	PlanID     string `json:"plan_id"`
	Status     string `json:"status"`
	StartsAt   int64  `json:"starts_at,omitempty"` // epoch 秒
	EndsAt     int64  `json:"ends_at,omitempty"`   // epoch 秒
}

// ClaimResult 领取结果
type ClaimResult struct {
	OK         bool         `json:"ok"`
	Code       int          `json:"code"`
	Message    string       `json:"message"`
	PlanID     string       `json:"plan_id"`
	PlanName   string       `json:"plan_name"`
	NextAt     int64        `json:"next_at"`     // 1005 时下次可领时间 epoch 毫秒
	ServerTime int64        `json:"server_time"` // 官方 data.server_time（毫秒）
	Plan       *ClaimedPlan `json:"plan,omitempty"`
}

// claimFailMessages 业务错误码 → 中文文案（与 zcode-switch i18n 一致）
var claimFailMessages = map[int]string{
	1001: "套餐不存在",
	1002: "活动已结束或套餐暂不可领取",
	1003: "该套餐已经领取过",
	1004: "不符合领取条件",
	1005: "今日领取名额已用完",
	3001: "领取参数错误，请刷新后重试",
	3007: "验证码校验失败，请重试",
	401:  "请先登录后再领取",
}

func claimFailureMessage(code int, body map[string]interface{}) string {
	base, ok := claimFailMessages[code]
	if !ok {
		base = "领取失败"
	}
	serverMsg := ""
	if body != nil {
		serverMsg = firstNonEmpty(jsonStr(body, "msg"), jsonStr(body, "message"))
	}
	if serverMsg != "" {
		return fmt.Sprintf("%s（%s）", base, serverMsg)
	}
	return base
}

// PreviewPlans 检测当前可领取的活动列表（按 priority 降序）
func (z *ZCodeAPI) PreviewPlans(a *Account) ([]ActivityPlan, error) {
	token := z.billingToken(a)
	if token == "" {
		return nil, fmt.Errorf("账号缺少 JWT 凭证，无法检测活动")
	}
	urlStr := fmt.Sprintf("%s?app_version=%s&platform=%s", BillingPreviewURL, z.appVersion, ClientPlatform())
	resp, err := z.doGetJSON(a, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("活动预览请求失败: %w", err)
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("鉴权失败 HTTP %d（凭证可能已失效）", resp.StatusCode)
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("活动预览响应解析失败: %s", truncate(resp.RawText, 200))
	}
	if code := jsonInt(resp.Body, "code"); code != 0 {
		return nil, fmt.Errorf("活动预览失败: %s", claimFailureMessage(code, resp.Body))
	}
	data, _ := resp.Body["data"].(map[string]interface{})
	if data == nil {
		return nil, nil
	}
	plansRaw, _ := data["plans"].([]interface{})
	var out []ActivityPlan
	for _, p := range plansRaw {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if ap := parseActivityPlan(pm); ap != nil {
			out = append(out, *ap)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].PlanID < out[j].PlanID
	})
	return out, nil
}

func parseActivityPlan(p map[string]interface{}) *ActivityPlan {
	planID := strings.TrimSpace(firstNonEmpty(jsonStr(p, "plan_id"), jsonStr(p, "planId")))
	if planID == "" {
		return nil
	}
	ap := &ActivityPlan{
		PlanID:      planID,
		Name:        strings.TrimSpace(jsonStr(p, "name")),
		Description: strings.TrimSpace(jsonStr(p, "description")),
		Priority:    jsonInt(p, "priority"),
	}
	if ap.Priority < 0 {
		ap.Priority = 0
	}
	// entitlements[] → 只保留 model_usage/token 的赠送项
	if ents, ok := p["entitlements"].([]interface{}); ok {
		for _, e := range ents {
			em, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			meter := firstNonEmpty(jsonStr(em, "meter"))
			unitType := firstNonEmpty(jsonStr(em, "unit_type"), jsonStr(em, "unitType"))
			showName := strings.TrimSpace(firstNonEmpty(jsonStr(em, "show_name"), jsonStr(em, "showName")))
			if meter != "model_usage" || unitType != "token" || showName == "" {
				continue
			}
			units := 0.0
			if n := jsonNum(em, "grant_units", "grantUnits"); n != nil {
				units = *n
			}
			period := firstNonEmpty(jsonStr(em, "period"), "one_time")
			gi := GrantItem{Name: showName, Units: units, Period: period, Priority: jsonInt(em, "priority")}
			// 官方附加字段：capabilities[]、effective_at
			if caps, ok := em["capabilities"].([]interface{}); ok {
				for _, c := range caps {
					if cs, ok := c.(string); ok && cs != "" {
						gi.Capabilities = append(gi.Capabilities, cs)
					}
				}
			}
			if ea := jsonNum(em, "effective_at", "effectiveAt"); ea != nil {
				gi.EffectiveAt = int64(*ea)
			}
			ap.GrantItems = append(ap.GrantItems, gi)
			ap.Grants = append(ap.Grants, fmt.Sprintf("%s · %s Token（%s）", showName, formatUnits(units), periodLabelCN(period)))
		}
	}
	return ap
}

func periodLabelCN(period string) string {
	switch period {
	case "daily":
		return "每日"
	case "weekly":
		return "每周"
	case "monthly":
		return "每月"
	}
	return "一次性"
}

// formatUnits 亿/万 缩写（claim.rs fmt_units 移植）
func formatUnits(n float64) string {
	trim := func(x float64) string {
		r := math_Round(x*10) / 10
		if r == float64(int64(r)) {
			return fmt.Sprintf("%d", int64(r))
		}
		return fmt.Sprintf("%.1f", r)
	}
	switch {
	case n >= 1e8:
		return trim(n/1e8) + "亿"
	case n >= 1e4:
		return trim(n/1e4) + "万"
	}
	return fmt.Sprintf("%d", int64(math_Round(n)))
}

func math_Round(x float64) float64 {
	if x < 0 {
		return -float64(int64(-x + 0.5))
	}
	return float64(int64(x + 0.5))
}

// SubmitClaim 提交领取。captchaParam 为空 = GetVerifyParam 判定免验证
// （captcha_mode=off 或上游未开启验证码），此时不带头直连——上游若仍要求
// 验证码会返回 3001/3007，由调用方按既有重解路径处理。
func (z *ZCodeAPI) SubmitClaim(a *Account, planID, captchaParam, captchaRegion string) *ClaimResult {
	result := &ClaimResult{PlanID: planID}
	token := z.billingToken(a)
	if token == "" {
		result.Code = -1
		result.Message = "账号缺少 JWT 凭证"
		return result
	}

	body, _ := json.Marshal(map[string]string{"plan_id": planID})
	client := ClientForURL(z.egress.ProxyURLForAccount(a), BillingClaimURL, 25*time.Second)
	req, err := http.NewRequest("POST", BillingClaimURL, bytes.NewReader(body))
	if err != nil {
		result.Code = -1
		result.Message = err.Error()
		return result
	}
	id := NewClientIdentity(z.appVersion, a.DeviceMid)
	for k, v := range ZaiClientHeaders(id) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if p := strings.TrimSpace(captchaParam); p != "" {
		req.Header.Set("X-Aliyun-Captcha-Verify-Param", p)
	}
	if r := strings.TrimSpace(captchaRegion); r != "" {
		req.Header.Set("X-Aliyun-Captcha-Verify-Region", r)
	}

	resp, err := client.Do(req)
	if err != nil {
		result.Code = -1
		result.Message = fmt.Sprintf("领取请求失败: %v", err)
		return result
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v map[string]interface{}
	json.Unmarshal(raw, &v)

	code := jsonInt(v, "code")
	if resp.StatusCode >= 400 && code == -1 {
		code = resp.StatusCode
	}
	result.Code = code
	// 官方 data.server_time（秒→毫秒）
	if data, ok := v["data"].(map[string]interface{}); ok {
		if st := jsonNum(data, "server_time"); st != nil {
			result.ServerTime = int64(*st) * 1000
		}
	}
	if code == 0 {
		result.OK = true
		result.Message = "领取成功"
		// 官方 data.plan: {user_plan_id, plan_id, status, starts_at, ends_at}
		if data, ok := v["data"].(map[string]interface{}); ok {
			if plan, ok := data["plan"].(map[string]interface{}); ok {
				cp := &ClaimedPlan{
					UserPlanID: jsonStr(plan, "user_plan_id"),
					PlanID:     firstNonEmpty(jsonStr(plan, "plan_id"), planID),
					Status:     jsonStr(plan, "status"),
				}
				if n := jsonNum(plan, "starts_at"); n != nil {
					cp.StartsAt = int64(*n)
				}
				if n := jsonNum(plan, "ends_at"); n != nil {
					cp.EndsAt = int64(*n)
				}
				result.Plan = cp
				result.PlanName = firstNonEmpty(jsonStr(plan, "name"), cp.PlanID)
			}
		}
		if result.PlanName == "" {
			result.PlanName = planID
		}
		return result
	}
	result.Message = claimFailureMessage(code, v)
	// 1005: 今日名额用完，提取下次可领时间（官方 failureEndsAt = data.plan.ends_at 秒）
	if code == 1005 {
		if data, ok := v["data"].(map[string]interface{}); ok {
			if plan, ok := data["plan"].(map[string]interface{}); ok {
				if ends := jsonNum(plan, "ends_at"); ends != nil && *ends > 0 {
					result.NextAt = int64(*ends) * 1000
				}
			}
		}
	}
	return result
}

// ReportActivation 上报激活事件（app_launch + app_daily_active），触发服务端 Start Plan 授予
func (z *ZCodeAPI) ReportActivation(a *Account) error {
	userID := z.telemetryUserID(a)
	if userID == "" {
		return fmt.Errorf("无法确定 user_id（user_info 缺失）")
	}
	mid := a.DeviceMid
	if mid == "" {
		mid = LocalDeviceMid()
	}
	client := ClientForURL(z.egress.ProxyURLForAccount(a), EventReportURL, 15*time.Second)
	for _, element := range []string{"app_launch", "app_daily_active"} {
		payload := map[string]interface{}{
			"event_id":           uuid.NewString(),
			"client_timezone":    clientTimezoneValue(),
			"client_language":    zcodeLang,
			"element_name":       element,
			"event_region":       "app",
			"event_type":         "view",
			"event_text":         "",
			"event_extra_detail": map[string]interface{}{},
			"user_id":            userID,
			"screen_resolution":  screenResolution,
			"app_version":        z.appVersion,
			"device_os_category": osCategoryValue(),
			"device_os_version":  cachedOSVer,
			"device_mid":         mid,
			"mac_id":             "",
			"marketing_params":   "{}",
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", EventReportURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		id := NewClientIdentity(z.appVersion, mid)
		for k, v := range ZaiClientHeaders(id) {
			req.Header.Set(k, v)
		}
		if token := z.billingToken(a); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("激活事件 %s 上报失败: %w", element, err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var v map[string]interface{}
		json.Unmarshal(raw, &v)
		if code := jsonInt(v, "code"); code != 0 && code != -1 {
			return fmt.Errorf("激活事件 %s 被拒: %s", element, claimFailureMessage(code, v))
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("激活事件 %s HTTP %d: %s", element, resp.StatusCode, truncate(string(raw), 150))
		}
	}
	return nil
}

// telemetryUserID 从 user_info 提取 user_id（claim.rs telemetry_user_id 移植）
func (z *ZCodeAPI) telemetryUserID(a *Account) string {
	if a.UserInfo != "" {
		var ui map[string]interface{}
		if json.Unmarshal([]byte(a.UserInfo), &ui) == nil {
			if id := firstNonEmpty(jsonStr(ui, "id"), jsonStr(ui, "user_id")); id != "" {
				return id
			}
		}
	}
	// 兜底：解 access_token / zcode_jwt 的 JWT payload
	for _, tok := range []string{a.AccessToken, a.ZCodeJWT} {
		if tok == "" {
			continue
		}
		if claims, err := DecodeJWTPayload(tok); err == nil {
			if id := firstNonEmpty(jsonStr(claims, "user_id"), jsonStr(claims, "sub")); id != "" {
				return id
			}
		}
	}
	return a.UserID
}

// ClaimForAccount 组合流程：检测活动 → 选优先级最高 → 求解验证码 → 领取 → 落记录。
// 账号级互斥：官方客户端用跨窗口广播锁防重复领取（createBroadcastService），
// 这里用 per-account mutex 达到同等效果（UI 手动与 cron 计划并发时不会双领）。
func (z *ZCodeAPI) ClaimForAccount(a *Account) *ClaimResult {
	mu := z.claimLockFor(a.ID)
	if !mu.TryLock() {
		return &ClaimResult{Code: -1, Message: "该账号已有领取任务在执行中（本地互斥）"}
	}
	defer mu.Unlock()
	return z.claimForAccountLocked(a)
}

func (z *ZCodeAPI) claimLockFor(id int64) *sync.Mutex {
	z.claimMu.Lock()
	defer z.claimMu.Unlock()
	if m, ok := z.claimLocks[id]; ok {
		return m
	}
	m := &sync.Mutex{}
	z.claimLocks[id] = m
	return m
}

func (z *ZCodeAPI) claimForAccountLocked(a *Account) *ClaimResult {
	plans, err := z.PreviewPlans(a)
	record := &ClaimRecord{AccountID: a.ID, Email: a.Email, TaskType: "claim"}
	if err != nil {
		record.Success = false
		record.Message = err.Error()
		if err := z.db.InsertClaimRecord(record); err != nil {
			log.Printf("[activity] insert claim record: %v", err)
		}
		return &ClaimResult{Code: -1, Message: err.Error()}
	}
	if len(plans) == 0 {
		// 无活动视为成功空跑（与 detect 语义一致，避免调度统计 0/N 误报 failed）
		record.Success = true
		record.Message = "当前无可领取活动"
		if err := z.db.InsertClaimRecord(record); err != nil {
			log.Printf("[activity] insert claim record: %v", err)
		}
		return &ClaimResult{OK: true, Code: 0, Message: "当前无可领取活动"}
	}
	plan := plans[0] // 已按 priority 降序
	record.PlanID = plan.PlanID
	record.PlanName = plan.Name

	// 求解阿里云验证码
	captchaParam, region, err := z.captcha.GetVerifyParam(a)
	if err != nil {
		record.Message = fmt.Sprintf("验证码求解失败: %v", err)
		if err := z.db.InsertClaimRecord(record); err != nil {
			log.Printf("[activity] insert claim record: %v", err)
		}
		z.db.SetAccountClaimResult(a.ID, plan.Name, record.Message)
		return &ClaimResult{Code: -1, PlanID: plan.PlanID, PlanName: plan.Name, Message: record.Message}
	}

	result := z.SubmitClaim(a, plan.PlanID, captchaParam, region)
	// 3001/3007: 参数错/验证码失败 → 失效缓存重解验证码后重试一次
	if result.Code == 3001 || result.Code == 3007 {
		z.captcha.InvalidateFor(a)
		if p2, r2, err := z.captcha.GetVerifyParam(a); err == nil {
			result = z.SubmitClaim(a, plan.PlanID, p2, r2)
		}
	}
	// 1003: 已领取过 → 视为幂等成功（与 detect 空跑语义一致，避免调度统计误报 failed；record.Code 保留 1003）
	if result.Code == 1003 {
		result.OK = true
		result.Message = "该套餐已经领取过（幂等成功）"
	}
	result.PlanName = firstNonEmpty(result.PlanName, plan.Name)
	record.Success = result.OK
	record.Code = result.Code
	record.Message = result.Message
	record.NextAt = result.NextAt
	if err := z.db.InsertClaimRecord(record); err != nil {
		log.Printf("[activity] insert claim record: %v", err)
	}
	z.db.SetAccountClaimResult(a.ID, result.PlanName, result.Message)

	// 领取成功后异步刷新额度
	if result.OK {
		z.goBackground("claim-quota-refresh", func() {
			time.Sleep(2 * time.Second)
			z.RefreshAccountQuota(a)
		})
	}
	return result
}

// DetectForAccount 仅检测活动（不领取）
func (z *ZCodeAPI) DetectForAccount(a *Account) *ClaimResult {
	plans, err := z.PreviewPlans(a)
	record := &ClaimRecord{AccountID: a.ID, Email: a.Email, TaskType: "detect"}
	if err != nil {
		record.Message = err.Error()
		if err := z.db.InsertClaimRecord(record); err != nil {
			log.Printf("[activity] insert claim record: %v", err)
		}
		return &ClaimResult{Code: -1, Message: err.Error()}
	}
	if len(plans) == 0 {
		record.Success = true
		record.Message = "无可领取活动"
		if err := z.db.InsertClaimRecord(record); err != nil {
			log.Printf("[activity] insert claim record: %v", err)
		}
		return &ClaimResult{OK: true, Message: "无可领取活动"}
	}
	names := make([]string, 0, len(plans))
	for _, p := range plans {
		names = append(names, p.Name)
	}
	record.Success = true
	record.PlanID = plans[0].PlanID
	record.PlanName = strings.Join(names, "、")
	record.Message = fmt.Sprintf("发现 %d 个活动: %s", len(plans), record.PlanName)
	if err := z.db.InsertClaimRecord(record); err != nil {
		log.Printf("[activity] insert claim record: %v", err)
	}
	return &ClaimResult{OK: true, PlanID: plans[0].PlanID, PlanName: record.PlanName, Message: record.Message}
}

// ActivateForAccount 激活流程：上报激活事件 → 刷新额度确认套餐生效
func (z *ZCodeAPI) ActivateForAccount(a *Account) *ClaimResult {
	record := &ClaimRecord{AccountID: a.ID, Email: a.Email, TaskType: "activate"}
	if err := z.ReportActivation(a); err != nil {
		record.Message = err.Error()
		if err := z.db.InsertClaimRecord(record); err != nil {
			log.Printf("[activity] insert claim record: %v", err)
		}
		return &ClaimResult{Code: -1, Message: err.Error()}
	}
	// 上报后刷新额度确认
	time.Sleep(1500 * time.Millisecond)
	ov, err := z.FetchQuotaRaw(a)
	if err != nil {
		record.Success = true
		record.Message = "激活事件已上报；额度确认失败: " + err.Error()
		if err := z.db.InsertClaimRecord(record); err != nil {
			log.Printf("[activity] insert claim record: %v", err)
		}
		return &ClaimResult{OK: true, Message: record.Message}
	}
	z.applyQuotaResult(a, ov)
	if ov.AuthFailed {
		record.Message = "激活事件已上报，但凭证已失效（账号被标记 invalid，若配有 refresh_token 将自动恢复后可重试）"
		z.db.InsertClaimRecord(record)
		return &ClaimResult{OK: false, Message: record.Message}
	}
	if ov.PlanTier != "" {
		record.Success = true
		record.PlanName = ov.PlanTier
		record.Message = fmt.Sprintf("激活成功，当前套餐: %s", ov.PlanTier)
	} else {
		record.Message = "激活事件已上报，但未检测到生效套餐（服务端可能延迟授予）"
	}
	if err := z.db.InsertClaimRecord(record); err != nil {
		log.Printf("[activity] insert claim record: %v", err)
	}
	return &ClaimResult{OK: record.Success, PlanName: ov.PlanTier, Message: record.Message}
}

// truncate 按 rune 截断，避免切碎 UTF-8 多字节字符
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
