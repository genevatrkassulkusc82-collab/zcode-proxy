package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- 额度查询与归一化 ----
// 两个通道：
//   jwt    → zcode.z.ai /api/v1/zcode-plan/billing/balance（current 已废弃仅兜底）
//   apikey → api.z.ai /api/monitor/usage/quota/limit + /api/biz/subscription/list（Authorization 直传 Key）
// 归一化逻辑移植 zcode-switch quota.rs（plans/balances 双结构 + 防御式字段别名）。

const (
	BillingBaseURL    = "https://zcode.z.ai/api/v1/zcode-plan"
	SubscriptionURL   = "https://api.z.ai/api/biz/subscription/list"
	QuotaLimitURL     = "https://api.z.ai/api/monitor/usage/quota/limit"
	BillingPreviewURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/preview"
	BillingClaimURL   = "https://zcode.z.ai/api/v1/zcode-plan/billing/claim"
	EventReportURL    = "https://zcode.z.ai/api/v1/event/report"
	ClientConfigsURL  = "https://zcode.z.ai/api/v1/client/configs"
)

// QuotaItem 单条额度切片
type QuotaItem struct {
	Name        string   `json:"name"`
	Total       *float64 `json:"total"`
	Used        *float64 `json:"used"`
	Remaining   *float64 `json:"remaining"`
	PercentUsed *float64 `json:"percent_used"`
	Unit        string   `json:"unit"`
	PeriodEnd   string   `json:"period_end"`
}

// QuotaOverview 归一化额度总览
// QuotaPlanSlot 单个套餐槽位（plans[] 与其 balances 明细）
type QuotaPlanSlot struct {
	PlanID      string      `json:"plan_id"`
	Name        string      `json:"name"`
	Tier        string      `json:"tier"`
	Status      string      `json:"status"`
	Expire      string      `json:"expire"`
	Total       *float64    `json:"total"`
	Used        *float64    `json:"used"`
	Remaining   *float64    `json:"remaining"`
	PercentUsed *float64    `json:"percent_used"`
	Items       []QuotaItem `json:"items"`
}

type QuotaOverview struct {
	PlanTier    string          `json:"plan_tier"`
	PlanExpire  string          `json:"plan_expire"`
	Total       *float64        `json:"total"`
	Used        *float64        `json:"used"`
	Remaining   *float64        `json:"remaining"`
	PercentUsed *float64        `json:"percent_used"`
	Items       []QuotaItem     `json:"items"`
	Plans       []QuotaPlanSlot `json:"plans"`
	Source      string          `json:"source"`
	RefreshedAt int64           `json:"refreshed_at"`
	NotEntitled bool            `json:"not_entitled"` // 无 Coding Plan / 未激活
	AuthFailed  bool            `json:"auth_failed"`  // 401/403 凭证失效
	IsEmpty     bool            `json:"is_empty"`
	NextReset   int64           `json:"next_reset,omitempty"` // 最早重置时间（unix 秒，monitor 通道）
	Channels    []QuotaChannel  `json:"channels,omitempty"`   // 双通道额度构成（交叉核对时填充）
}

// QuotaChannel 单通道额度概要（F4：Start Plan 计费 vs coding plan monitor 并排可见）
type QuotaChannel struct {
	Source     string  `json:"source"`
	PlanTier   string  `json:"plan_tier"`
	Remaining  float64 `json:"remaining"`
	Exhausted  bool    `json:"exhausted"`
	AuthFailed bool    `json:"auth_failed"`
	NextReset  int64   `json:"next_reset,omitempty"`
}

// channelSummary 从 overview 提取通道概要
func channelSummary(ov *QuotaOverview) QuotaChannel {
	c := QuotaChannel{Source: ov.Source, PlanTier: ov.PlanTier, Exhausted: ov.AllExhausted(), NextReset: ov.NextReset}
	if ov.Remaining != nil {
		c.Remaining = *ov.Remaining
	}
	return c
}

// quotaResult 内部：HTTP + 业务码 + 原始 JSON
type apiResponse struct {
	StatusCode int
	Body       map[string]interface{}
	RawText    string
}

// doGetJSON 带客户端身份头的 GET 请求（billing 端点：Bearer JWT）
func (z *ZCodeAPI) doGetJSON(a *Account, urlStr string, extraHeaders map[string]string) (*apiResponse, error) {
	token := z.billingToken(a)
	if token == "" {
		return nil, fmt.Errorf("账号缺少有效凭证")
	}
	return z.doGetJSONAuth(a, urlStr, "Bearer "+token, extraHeaders)
}

// doGetJSONAuth 同 doGetJSON，但显式指定完整 Authorization 头值。
// monitor 端点（quota/limit、subscription/list）要求直传 API Key、不带 Bearer 前缀
// （对齐 zai-org/ZCode createBigModelUsageHeaders）；authValue 为空时回退 billingToken。
func (z *ZCodeAPI) doGetJSONAuth(a *Account, urlStr, authValue string, extraHeaders map[string]string) (*apiResponse, error) {
	client := ClientForURL(z.egress.ProxyURLForAccount(a), urlStr, 25*time.Second)
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	id := NewClientIdentity(z.appVersion, a.DeviceMid)
	for k, v := range ZaiClientHeaders(id) {
		req.Header.Set(k, v)
	}
	if authValue == "" {
		token := z.billingToken(a)
		if token == "" {
			return nil, fmt.Errorf("账号缺少有效凭证")
		}
		authValue = "Bearer " + token
	}
	req.Header.Set("Authorization", authValue)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	out := &apiResponse{StatusCode: resp.StatusCode, RawText: string(body)}
	if len(body) > 0 {
		var v map[string]interface{}
		if json.Unmarshal(body, &v) == nil {
			out.Body = v
		}
	}
	return out, nil
}

// billingToken 选择计费接口凭证（quota.rs zai_billing_token 简化版）：
// JWT 账号用 zcode_jwt；API Key 账号用 api_key
func (z *ZCodeAPI) billingToken(a *Account) string {
	if a.ZCodeJWT != "" {
		return a.ZCodeJWT
	}
	return a.APIKey
}

func businessOK(v map[string]interface{}) bool {
	if v == nil {
		return false
	}
	if success, ok := v["success"].(bool); ok && !success {
		return false
	}
	if _, has := v["code"]; !has {
		return true
	}
	n := jsonInt(v, "code")
	return n == 0 || n == 200
}

func jsonInt(v map[string]interface{}, key string) int {
	switch x := v[key].(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return -1
}

func jsonStr(v map[string]interface{}, key string) string {
	if s, ok := v[key].(string); ok {
		return s
	}
	return ""
}

func jsonNum(v map[string]interface{}, keys ...string) *float64 {
	for _, k := range keys {
		switch x := v[k].(type) {
		case float64:
			if !math.IsNaN(x) && !math.IsInf(x, 0) {
				val := x
				return &val
			}
		case string:
			t := strings.ReplaceAll(x, ",", "")
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return &f
			}
		}
	}
	return nil
}

// unwrapData 递归解包 data/result 层（最多 4 层）
func unwrapData(v map[string]interface{}) map[string]interface{} {
	cur := v
	for i := 0; i < 4; i++ {
		if d, ok := cur["data"].(map[string]interface{}); ok {
			cur = d
			continue
		}
		if r, ok := cur["result"].(map[string]interface{}); ok {
			cur = r
			continue
		}
		break
	}
	return cur
}

// ---- 主入口 ----

// FetchQuotaRaw 拉取并归一化账号额度（不落库、不改状态）
// 状态约定：纯网络失败/业务失败返回 err（调用方仅记日志，下轮重试）；
// 仅当已应答通道全部返回 401/403 时返回 AuthFailed 结果（nil err），由状态机标记 invalid。
func (z *ZCodeAPI) FetchQuotaRaw(a *Account) (*QuotaOverview, error) {
	if a.ZCodeJWT != "" {
		ov, err := z.fetchZaiBilling(a)
		if err == nil {
			// JWT 通道（Start Plan 计费）报耗尽而账号带 API Key 时交叉核对
			// monitor 通道（individual coding plan 额度在彼处）：monitor 有余量
			// 则以 monitor 为准，避免把可用的 coding plan 账号误标 exhausted
			if ov != nil && ov.AllExhausted() && a.APIKey != "" {
				if ov2, err2 := z.fetchApiZaiMonitor(a); err2 == nil && ov2 != nil &&
					!ov2.AllExhausted() && !ov2.IsEmpty && !ov2.AuthFailed {
					log.Printf("[quota] account %s: JWT billing exhausted but monitor channel has quota, using monitor", a.Email)
					// F4：双通道额度构成并排入库（quota_json.channels）
					ov2.Channels = append(ov2.Channels, channelSummary(ov), channelSummary(ov2))
					return ov2, nil
				}
			}
			return ov, nil
		}
		// 纯网络失败（超时/连接拒绝，无 HTTP 响应）：不回退、不改状态
		if ov == nil || !ov.AuthFailed {
			return nil, err
		}
		// JWT 通道明确 401/403 且账号带 API Key 时回退 monitor 通道
		if a.APIKey != "" {
			ov2, err2 := z.fetchApiZaiMonitor(a)
			if err2 != nil && (ov2 == nil || !ov2.AuthFailed) {
				// 回退通道网络/业务失败：单通道鉴权失败不定性，不改状态
				return nil, err2
			}
			// 回退成功，或双通道均 401/403（AuthFailed → 状态机标记 invalid）
			return ov2, nil
		}
		// 无回退通道：JWT 通道 401/403 即"已应答通道全部失败"，
		// 必须按约定返回 AuthFailed + nil err，否则后台/手动刷新永远无法
		// 把过期账号迁移到 invalid（或触发 refresh_token 兑换）
		return &QuotaOverview{AuthFailed: true}, nil
	}
	if a.APIKey != "" {
		ov, err := z.fetchApiZaiMonitor(a)
		if ov != nil && ov.AuthFailed {
			// 唯一通道 401/403：同样定性为 AuthFailed（去 err 化，交状态机）。
			// monitor 通道的 AuthFailed 一律伴随非 nil err，故这里不看 err。
			return ov, nil
		}
		return ov, err
	}
	return nil, fmt.Errorf("账号缺少凭证")
}

// fetchZaiBilling JWT 通道：billing/balance（现行端点，官方客户端唯一在用）优先，
// billing/current 已废弃仅兜底
func (z *ZCodeAPI) fetchZaiBilling(a *Account) (*QuotaOverview, error) {
	urls := []string{
		fmt.Sprintf("%s/billing/balance?app_version=%s", BillingBaseURL, z.appVersion),
		fmt.Sprintf("%s/billing/current?app_version=%s", BillingBaseURL, z.appVersion),
	}
	var primary, last *apiResponse
	for i, u := range urls {
		resp, err := z.doGetJSON(a, u, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return &QuotaOverview{AuthFailed: true}, fmt.Errorf("HTTP %d 鉴权失败", resp.StatusCode)
		}
		if resp.Body != nil {
			if code := jsonInt(resp.Body, "code"); code == 401 {
				return &QuotaOverview{AuthFailed: true}, fmt.Errorf("业务码 401 令牌失效")
			}
		}
		if resp.StatusCode == 200 && businessOK(resp.Body) {
			ov := normalizeBalanceResponse(resp.Body, "zcode.z.ai/billing")
			ov.RefreshedAt = time.Now().Unix()
			return ov, nil
		}
		if i == 0 {
			primary = resp
		}
		last = resp
	}
	msg := extractErrMsg(primary.Body)
	if msg == "" {
		msg = extractErrMsg(last.Body)
	}
	if strings.Contains(msg, "不存在coding plan") || strings.Contains(msg, "没有资格") {
		return &QuotaOverview{NotEntitled: true, IsEmpty: true}, nil
	}
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", last.StatusCode)
	}
	return nil, fmt.Errorf("额度查询失败: %s", msg)
}

// fetchApiZaiMonitor API Key 通道：quota/limit + subscription/list
// monitor 端点以 API Key 直传 Authorization（无 Bearer 前缀，对齐官方客户端）
func (z *ZCodeAPI) fetchApiZaiMonitor(a *Account) (*QuotaOverview, error) {
	if a.APIKey == "" {
		return nil, fmt.Errorf("账号缺少 API Key")
	}
	resp, err := z.doGetJSONAuth(a, QuotaLimitURL, a.APIKey, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return &QuotaOverview{AuthFailed: true}, fmt.Errorf("HTTP %d 鉴权失败", resp.StatusCode)
	}
	if !businessOK(resp.Body) {
		code := jsonInt(resp.Body, "code")
		if code == 401 {
			return &QuotaOverview{AuthFailed: true}, fmt.Errorf("业务码 401 令牌失效")
		}
		msg := extractErrMsg(resp.Body)
		if strings.Contains(msg, "不存在coding plan") || strings.Contains(msg, "没有资格") {
			return &QuotaOverview{NotEntitled: true, IsEmpty: true}, nil
		}
		return nil, fmt.Errorf("额度查询失败: %s", msg)
	}
	var sub *map[string]interface{}
	if subResp, err := z.doGetJSONAuth(a, SubscriptionURL, a.APIKey, nil); err == nil && businessOK(subResp.Body) {
		body := subResp.Body
		sub = &body
	}
	ov := normalizeQuotaLimit(resp.Body, sub)
	ov.Source = "api.z.ai/monitor"
	ov.RefreshedAt = time.Now().Unix()
	// F3：取各限额桶最早的 nextResetTime（毫秒 → unix 秒），供耗尽提示/未来调度
	if d, ok := resp.Body["data"].(map[string]interface{}); ok {
		if limits, ok := d["limits"].([]interface{}); ok {
			for _, it := range limits {
				lm, ok := it.(map[string]interface{})
				if !ok {
					continue
				}
				if n, ok := lm["nextResetTime"].(float64); ok && n > 0 {
					if s := int64(n / 1000); ov.NextReset == 0 || s < ov.NextReset {
						ov.NextReset = s
					}
				}
			}
		}
	}
	return ov, nil
}

func extractErrMsg(v map[string]interface{}) string {
	if v == nil {
		return ""
	}
	for _, k := range []string{"msg", "message", "error"} {
		if s := jsonStr(v, k); s != "" {
			return s
		}
	}
	return ""
}

// ---- 归一化：billing/current & billing/balance（zcode.z.ai）----

func normalizeBalanceResponse(raw map[string]interface{}, source string) *QuotaOverview {
	data := unwrapData(raw)
	ov := &QuotaOverview{Source: source}

	// plans[]：建槽位并取 active 套餐推断 tier / expire
	slots := map[string]*QuotaPlanSlot{}
	var slotOrder []string
	var activePlan map[string]interface{}
	if plans, ok := data["plans"].([]interface{}); ok {
		for _, p := range plans {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			pid := jsonStr(pm, "plan_id")
			if pid == "" {
				continue
			}
			pname := jsonStr(pm, "name")
			status := jsonStr(pm, "status")
			if _, exists := slots[pid]; !exists {
				slots[pid] = &QuotaPlanSlot{
					PlanID: pid, Name: pname, Status: status,
					Tier:   PlanTierFromID(pid, pname),
					Expire: ExtractExpire(pm),
				}
				slotOrder = append(slotOrder, pid)
			}
			if strings.EqualFold(status, "active") && activePlan == nil {
				activePlan = pm
			}
		}
		if activePlan == nil && len(plans) > 0 {
			activePlan, _ = plans[0].(map[string]interface{})
		}
	}
	if activePlan != nil {
		pid := jsonStr(activePlan, "plan_id")
		name := jsonStr(activePlan, "name")
		ov.PlanTier = PlanTierFromID(pid, name)
		ov.PlanExpire = ExtractExpire(activePlan)
	}

	// balances[]：逐模型额度切片
	if balances, ok := data["balances"].([]interface{}); ok {
		var totalSum, usedSum, remSum float64
		hasTotal, hasUsed, hasRem := false, false, false
		for _, b := range balances {
			bm, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			item := QuotaItem{
				Name:      firstNonEmpty(jsonStr(bm, "show_name"), jsonStr(bm, "name"), jsonStr(bm, "entitlement_id")),
				Total:     jsonNum(bm, "total_units", "total", "limit", "usage"),
				Used:      jsonNum(bm, "used_units", "used", "consumed", "currentValue"),
				Remaining: jsonNum(bm, "remaining_units", "available_units", "remaining", "available"),
				Unit:      firstNonEmpty(jsonStr(bm, "unit_type"), jsonStr(bm, "meter")),
				PeriodEnd: ExtractExpire(bm),
			}
			if item.Name == "" {
				item.Name = "额度"
			}
			// 三值互推
			if item.Remaining == nil && item.Total != nil && item.Used != nil {
				r := math.Max(*item.Total-*item.Used, 0)
				item.Remaining = &r
			}
			if item.Total == nil && item.Used != nil && item.Remaining != nil {
				t := *item.Used + *item.Remaining
				item.Total = &t
			}
			if item.Total != nil && item.Used != nil && *item.Total > 0 {
				p := math.Min(math.Max(*item.Used / *item.Total * 100, 0), 100)
				item.PercentUsed = &p
			}
			if item.Total != nil {
				totalSum += *item.Total
				hasTotal = true
			}
			if item.Used != nil {
				usedSum += *item.Used
				hasUsed = true
			}
			if item.Remaining != nil {
				remSum += *item.Remaining
				hasRem = true
			}
			// 归入对应套餐槽位（按 plan_id；无则单槽位或"其他"）
			bpid := firstNonEmpty(jsonStr(bm, "plan_id"), jsonStr(bm, "planId"))
			if bpid == "" && len(slotOrder) == 1 {
				bpid = slotOrder[0]
			}
			if bpid == "" {
				bpid = "__other__"
			}
			slot, exists := slots[bpid]
			if !exists {
				slot = &QuotaPlanSlot{PlanID: bpid, Name: "其他额度", Tier: PlanTierFromID(bpid, "")}
				slots[bpid] = slot
				slotOrder = append(slotOrder, bpid)
			}
			slot.Items = append(slot.Items, item)
			ov.Items = append(ov.Items, item)
		}
		if hasTotal {
			ov.Total = &totalSum
		}
		if hasUsed {
			ov.Used = &usedSum
		}
		if hasRem {
			ov.Remaining = &remSum
		}
		ov.IsEmpty = len(balances) == 0
	}

	// 顶层直给的 total/used/remaining（部分响应不带 balances）
	if ov.Total == nil {
		ov.Total = jsonNum(data, "total_units", "total")
	}
	if ov.Used == nil {
		ov.Used = jsonNum(data, "used_units", "used")
	}
	if ov.Remaining == nil {
		ov.Remaining = jsonNum(data, "available_units", "remaining_units", "remaining")
	}
	if ov.Total != nil && ov.Used != nil && *ov.Total > 0 {
		p := math.Min(math.Max(*ov.Used / *ov.Total * 100, 0), 100)
		ov.PercentUsed = &p
	}

	// 聚合套餐槽位（每个槽位由其明细求和）
	for _, pid := range slotOrder {
		slot := slots[pid]
		var t, u, r float64
		hasT, hasU, hasR := false, false, false
		for _, it := range slot.Items {
			if it.Total != nil {
				t += *it.Total
				hasT = true
			}
			if it.Used != nil {
				u += *it.Used
				hasU = true
			}
			if it.Remaining != nil {
				r += *it.Remaining
				hasR = true
			}
		}
		if hasT {
			slot.Total = &t
		}
		if hasU {
			slot.Used = &u
		}
		if hasR {
			slot.Remaining = &r
		}
		if slot.Total != nil && slot.Used != nil && *slot.Total > 0 {
			p := math.Min(math.Max(*slot.Used / *slot.Total * 100, 0), 100)
			slot.PercentUsed = &p
		}
		if slot.Name == "" {
			slot.Name = pid
		}
		ov.Plans = append(ov.Plans, *slot)
	}
	if ov.PlanExpire == "" {
		ov.PlanExpire = ExtractExpire(data)
	}
	return ov
}

// ---- 归一化：quota/limit + subscription/list（api.z.ai）----

func normalizeQuotaLimit(raw map[string]interface{}, sub *map[string]interface{}) *QuotaOverview {
	data := unwrapData(raw)
	ov := &QuotaOverview{}

	if level := jsonStr(data, "level"); level != "" {
		ov.PlanTier = tierFromLevel(level)
	}
	limits, _ := data["limits"].([]interface{})
	for _, l := range limits {
		lm, ok := l.(map[string]interface{})
		if !ok {
			continue
		}
		typ := jsonStr(lm, "type")
		unit := jsonInt(lm, "unit")
		number := jsonInt(lm, "number")
		period := unitLabel(unit, number)
		total := jsonNum(lm, "usage", "total")
		used := jsonNum(lm, "currentValue", "used")
		remaining := jsonNum(lm, "remaining")
		name := typ
		switch typ {
		case "TOKENS_LIMIT":
			name = fmt.Sprintf("提示次数（%s）", period)
		case "TIME_LIMIT":
			name = fmt.Sprintf("使用时长（%s）", period)
		}
		item := QuotaItem{
			Name:      name,
			Total:     total,
			Used:      used,
			Remaining: remaining,
			Unit:      typ,
		}
		if reset := jsonNum(lm, "nextResetTime"); reset != nil && *reset > 0 {
			item.PeriodEnd = formatEpochMsLocal(*reset) + " 重置"
		}
		if total != nil && used != nil && *total > 0 {
			p := math.Min(math.Max(*used / *total * 100, 0), 100)
			item.PercentUsed = &p
		} else if pct := jsonNum(lm, "percentage"); pct != nil {
			item.PercentUsed = pct
		}
		ov.Items = append(ov.Items, item)
		// 主切片：TIME_LIMIT 优先（分钟配额）
		if typ == "TIME_LIMIT" && total != nil && ov.Total == nil {
			ov.Total, ov.Used, ov.Remaining, ov.PercentUsed = total, used, remaining, item.PercentUsed
		}
	}
	if ov.Total == nil {
		for _, it := range ov.Items {
			if it.Total != nil {
				ov.Total, ov.Used, ov.Remaining, ov.PercentUsed = it.Total, it.Used, it.Remaining, it.PercentUsed
				break
			}
		}
	}
	ov.IsEmpty = len(limits) == 0

	// subscription/list 补套餐名与到期（响应形如 {"code":200,"data":[{...}]}）
	if sub != nil {
		arr, _ := (*sub)["data"].([]interface{})
		var current map[string]interface{}
		for _, s := range arr {
			sm, ok := s.(map[string]interface{})
			if !ok {
				continue
			}
			valid := jsonStr(sm, "status") == "VALID"
			inPeriod := true
			if b, ok := sm["inCurrentPeriod"].(bool); ok {
				inPeriod = b
			}
			if valid && inPeriod {
				current = sm
				break
			}
		}
		if current == nil && len(arr) > 0 {
			current, _ = arr[0].(map[string]interface{})
		}
		if current != nil {
			if pn := jsonStr(current, "productName"); pn != "" {
				ov.PlanTier = tierFromLevel(pn)
			}
			ov.PlanExpire = ExtractExpire(current)
		}
	}
	return ov
}

func unitLabel(unit, number int) string {
	switch unit {
	case 3:
		if number <= 0 {
			number = 5
		}
		return fmt.Sprintf("每 %d 小时", number)
	case 4:
		return "每天"
	case 5:
		return "每月"
	case 6:
		return "每周"
	}
	return "每周期"
}

func tierFromLevel(level string) string {
	l := strings.ToLower(level)
	switch {
	case strings.Contains(l, "max"):
		return "Max"
	case strings.Contains(l, "pro"):
		return "Pro"
	case strings.Contains(l, "lite"):
		return "Lite"
	}
	return level
}

// PlanTierFromID 从 plan_id/name 推断套餐档位（quota.rs plan_tier_from_id 移植）
func PlanTierFromID(planID, name string) string {
	hay := strings.ToLower(planID + " " + name)
	switch {
	case strings.Contains(hay, "max"):
		return "Max"
	case strings.Contains(hay, "pro"):
		return "Pro"
	case strings.Contains(hay, "lite"):
		return "Lite"
	case strings.Contains(hay, "start"):
		return "Start Plan"
	}
	for _, kw := range []string{"trial", "taste", "experience", "gift", "weekend", "promo", "activity", "体验"} {
		if strings.Contains(hay, kw) {
			return "体验"
		}
	}
	if planID != "" {
		return planID
	}
	return name
}

// tierRank 档位排序（merge 时选主切片）
func tierRank(tier string) int {
	switch strings.ToLower(tier) {
	case "max":
		return 5
	case "pro":
		return 4
	case "lite":
		return 3
	case "start plan":
		return 2
	case "体验":
		return 1
	}
	return 0
}

// ExtractExpire 从对象中提取到期时间（quota.rs extract_expire 移植，支持 epoch 秒/毫秒与常见字符串）
func ExtractExpire(obj map[string]interface{}) string {
	keys := []string{"nextRenewTime", "expireTime", "expire_time", "endTime", "end_time",
		"expireAt", "expiredTime", "validEndTime", "expires_at", "expiresAt", "expired_at",
		"period_end", "nextResetTime", "ends_at"}
	for _, k := range keys {
		v, ok := obj[k]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case float64:
			if x > 1e12 {
				return formatEpochMsLocal(x)
			}
			if x > 1e9 {
				return time.Unix(int64(x), 0).Format("2006-01-02 15:04")
			}
		case string:
			t := strings.TrimSpace(x)
			if t == "" {
				continue
			}
			if n, err := strconv.ParseFloat(t, 64); err == nil {
				if n > 1e12 {
					return formatEpochMsLocal(n)
				}
				if n > 1e9 {
					return time.Unix(int64(n), 0).Format("2006-01-02 15:04")
				}
			}
			if ts, err := time.Parse(time.RFC3339, t); err == nil {
				return ts.Local().Format("2006-01-02 15:04")
			}
			t = strings.Replace(t, "T", " ", 1)
			if len(t) >= 16 && t[4] == '-' && t[7] == '-' {
				return t[:16]
			}
			if len(t) == 10 && t[4] == '-' && t[7] == '-' {
				return t
			}
			return t
		}
	}
	return ""
}

func formatEpochMsLocal(ms float64) string {
	return time.UnixMilli(int64(ms)).Format("2006-01-02 15:04")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---- 状态判定（quota.py 状态迁移移植）----

// AllExhausted 所有切片剩余额度均 <= 0
func (ov *QuotaOverview) AllExhausted() bool {
	if len(ov.Items) == 0 {
		return ov.Remaining != nil && *ov.Remaining <= 0
	}
	for _, it := range ov.Items {
		if it.Remaining == nil || *it.Remaining > 0 {
			return false
		}
	}
	return true
}

// SortItemsByRemaining 剩余多的在前（best_quota 展示）
func SortItemsByRemaining(items []QuotaItem) {
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := 0.0, 0.0
		if items[i].Remaining != nil {
			ri = *items[i].Remaining
		}
		if items[j].Remaining != nil {
			rj = *items[j].Remaining
		}
		return ri > rj
	})
}
