package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// ---- Coding Plan 配额手动重置（app.asar 审计发现）----
// base = zcode.z.ai；仅付费 Coding Plan 账号可用（Start Plan 返回 3101）。
// 额度耗尽时可消耗「5 小时窗口重置」或「周重置」机会恢复配额。
// 请求头（createCodingPlanResetHeaders 移植）：
//   Authorization: Bearer <zcodejwttoken>
//   X-Bigmodel-Authorization: <coding-plan apiKey>
//   Bigmodel-Target-Type: PERSONAL | TEAM（TEAM 时附 Organization/Project）

const CodingPlanResetBase = "https://zcode.z.ai/api/v1/coding-plan/reset"

// ResetSlot 重置机会槽位
type ResetSlot struct {
	ExpireAt int64 `json:"expire_at"`
}

// ResetUsed 重置历史
type ResetUsed struct {
	UsedAt int64 `json:"used_at"`
}

// ResetStatus 重置状态
type ResetStatus struct {
	AvailableFiveHourResets []ResetSlot `json:"available_five_hour_resets"`
	AvailableWeekResets     []ResetSlot `json:"available_week_resets"`
	LatestFiveHourReset     *ResetUsed  `json:"latest_five_hour_reset_history"`
	LatestWeekReset         *ResetUsed  `json:"latest_week_reset_history"`
	HasUnreadHistory        bool        `json:"has_unread_history"`
}

// resetHeaders AC() 移植：双凭证 + 团队上下文
func (z *ZCodeAPI) resetHeaders(a *Account) map[string]string {
	h := map[string]string{
		"Authorization": "Bearer " + a.ZCodeJWT,
		"User-Agent":    "ZCode/" + z.appVersion,
		"accept":        "application/json",
	}
	if a.APIKey != "" {
		h["X-Bigmodel-Authorization"] = a.APIKey
	}
	// 团队上下文：账号备注/分组中以 team:orgId:projId 形式声明时启用
	if org, proj, ok := parseTeamContext(a); ok {
		h["Bigmodel-Target-Type"] = "TEAM"
		h["Bigmodel-Organization"] = org
		h["Bigmodel-Project"] = proj
	} else {
		h["Bigmodel-Target-Type"] = "PERSONAL"
	}
	return h
}

// parseTeamContext 从账号备注解析 team:<orgId>:<projId>
func parseTeamContext(a *Account) (org, proj string, ok bool) {
	var t struct {
		Team struct {
			Org  string `json:"org"`
			Proj string `json:"proj"`
		} `json:"team"`
	}
	if a.Remark != "" && json.Unmarshal([]byte(a.Remark), &t) == nil && t.Team.Org != "" && t.Team.Proj != "" {
		return t.Team.Org, t.Team.Proj, true
	}
	return "", "", false
}

func (z *ZCodeAPI) resetRequest(a *Account, method, path string, body map[string]interface{}) (map[string]interface{}, int, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	urlStr := CodingPlanResetBase + path
	req, err := http.NewRequest(method, urlStr, reader)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range z.resetHeaders(a) {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	client := ClientForURL(z.egress.ProxyURLForAccount(a), urlStr, 15*time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v map[string]interface{}
	json.Unmarshal(raw, &v)
	return v, resp.StatusCode, nil
}

// FetchResetStatus GET reset/status
func (z *ZCodeAPI) FetchResetStatus(a *Account) (*ResetStatus, int, string, error) {
	if a.ZCodeJWT == "" {
		return nil, 0, "", fmt.Errorf("需要 ZCode JWT")
	}
	v, status, err := z.resetRequest(a, "GET", "/status", nil)
	if err != nil {
		return nil, status, "", err
	}
	code := jsonInt(v, "code")
	if code != 0 {
		return nil, status, firstNonEmpty(jsonStr(v, "msg"), jsonStr(v, "message")), fmt.Errorf("业务码 %d: %s", code, firstNonEmpty(jsonStr(v, "msg"), jsonStr(v, "message")))
	}
	data, _ := v["data"].(map[string]interface{})
	if data == nil {
		return nil, status, "", fmt.Errorf("响应缺少 data")
	}
	raw, _ := json.Marshal(data)
	var st ResetStatus
	json.Unmarshal(raw, &st)
	return &st, status, "", nil
}

// UseReset POST reset/use {idempotency_key, reset_type}
func (z *ZCodeAPI) UseReset(a *Account, resetType string) (bool, int64, string, error) {
	idem := uuid.NewString()
	v, _, err := z.resetRequest(a, "POST", "/use", map[string]interface{}{
		"idempotency_key": idem,
		"reset_type":      resetType,
	})
	if err != nil {
		return false, 0, "", err
	}
	code := jsonInt(v, "code")
	msg := firstNonEmpty(jsonStr(v, "msg"), jsonStr(v, "message"))
	if code != 0 {
		return false, 0, msg, fmt.Errorf("业务码 %d: %s", code, msg)
	}
	data, _ := v["data"].(map[string]interface{})
	if d, ok := data["used"].(bool); ok {
		return d, 0, msg, nil
	}
	// 业务码 0 但响应未带 used 字段（响应形状逆向自 app.asar，无法保证该字段）：
	// 视为已消耗并告警——按失败记录会诱导上层重复消耗一次真实重置机会
	log.Printf("[reset] use %s: code=0 but data.used missing (msg=%q); treating as consumed", resetType, msg)
	return true, 0, msg, nil
}

// ResetForAccount 组合流程：查状态 → 选 five_hour 优先否则 week → use → 刷新额度 → 落记录。
// 账号级互斥：手动 + cron 并发时不会双重消耗重置机会。
func (z *ZCodeAPI) ResetForAccount(a *Account) *ClaimResult {
	mu := z.claimLockFor(a.ID)
	if !mu.TryLock() {
		return &ClaimResult{Code: -1, Message: "该账号已有领取/重置任务在执行中（本地互斥）"}
	}
	defer mu.Unlock()
	return z.resetForAccountLocked(a)
}

func (z *ZCodeAPI) resetForAccountLocked(a *Account) *ClaimResult {
	record := &ClaimRecord{AccountID: a.ID, Email: a.Email, TaskType: "reset"}
	st, _, bizMsg, err := z.FetchResetStatus(a)
	if err != nil {
		record.Message = fmt.Sprintf("重置状态查询失败: %v", err)
		if bizMsg != "" {
			record.Message = bizMsg
		}
		if err := z.db.InsertClaimRecord(record); err != nil {
			log.Printf("[reset] insert claim record: %v", err)
		}
		return &ClaimResult{Code: -1, Message: record.Message}
	}
	resetType := ""
	switch {
	case len(st.AvailableFiveHourResets) > 0:
		resetType = "FIVE_HOUR"
	case len(st.AvailableWeekResets) > 0:
		resetType = "WEEK"
	}
	if resetType == "" {
		record.Success = true
		record.Message = "无可用重置机会（five_hour/week 均已用完）"
		if err := z.db.InsertClaimRecord(record); err != nil {
			log.Printf("[reset] insert claim record: %v", err)
		}
		return &ClaimResult{OK: true, Message: record.Message}
	}
	used, _, msg, err := z.UseReset(a, resetType)
	if err != nil || !used {
		record.Message = fmt.Sprintf("重置执行失败(%s): %v %s", resetType, err, msg)
		if err := z.db.InsertClaimRecord(record); err != nil {
			log.Printf("[reset] insert claim record: %v", err)
		}
		z.db.SetAccountClaimResult(a.ID, "配额重置", record.Message)
		return &ClaimResult{Code: -1, Message: record.Message}
	}
	record.Success = true
	record.PlanName = "配额重置(" + resetType + ")"
	record.Message = "重置成功，配额已恢复"
	z.db.InsertClaimRecord(record)
	z.db.SetAccountClaimResult(a.ID, record.PlanName, record.Message)
	log.Printf("[reset] account %s quota reset via %s", a.Email, resetType)
	go func() {
		time.Sleep(2 * time.Second)
		z.RefreshAccountQuota(a)
	}()
	return &ClaimResult{OK: true, PlanName: record.PlanName, Message: record.Message}
}

// ---- 官方模型目录同步（client/configs）----

// CatalogModel 目录模型
type CatalogModel struct {
	ModelID       string `json:"modelId"`
	Name          string `json:"name"`
	ContextWindow int    `json:"contextWindow"`
	Priority      int    `json:"priority"`
	Vision        bool   `json:"vision"`
}

// SyncModelCatalog 拉取官方模型目录（builtinModels）并刷新 captcha 配置缓存
func (z *ZCodeAPI) SyncModelCatalog() ([]CatalogModel, error) {
	urlStr := fmt.Sprintf("%s?version=%s&os=%s", ClientConfigsURL, z.appVersion, NodePlatform())
	client := ClientForURL("", urlStr, 20*time.Second)
	req, _ := http.NewRequest("GET", urlStr, nil)
	id := NewClientIdentity(z.appVersion, "")
	for k, v := range ZaiClientHeaders(id) {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var v struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			BuiltinModels []CatalogModel `json:"builtinModels"`
			Providers     []struct {
				ID      string         `json:"id"`
				BaseURL string         `json:"baseUrl"`
				Models  []CatalogModel `json:"models"`
			} `json:"providers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("目录解析失败: %w", err)
	}
	if v.Code != 0 {
		return nil, fmt.Errorf("业务码 %d: %s", v.Code, v.Msg)
	}
	models := v.Data.BuiltinModels
	if len(models) == 0 {
		for _, p := range v.Data.Providers {
			models = append(models, p.Models...)
		}
	}
	// 缓存目录 + 去重
	seen := map[string]bool{}
	var uniq []CatalogModel
	for _, m := range models {
		if m.ModelID == "" || seen[m.ModelID] {
			continue
		}
		seen[m.ModelID] = true
		uniq = append(uniq, m)
	}
	if cj, err := json.Marshal(uniq); err == nil {
		z.db.SetSetting("model_catalog", string(cj))
	}
	log.Printf("[catalog] synced %d models from client/configs", len(uniq))
	return uniq, nil
}

// GetModelCatalog 读取缓存目录
func (z *ZCodeAPI) GetModelCatalog() []CatalogModel {
	raw, _ := z.db.GetSetting("model_catalog")
	if raw == "" {
		return nil
	}
	var out []CatalogModel
	json.Unmarshal([]byte(raw), &out)
	return out
}

// resetHistorySyncInterval 单账号上游重置历史同步最小间隔（防风控）
const resetHistorySyncInterval = 10 * time.Minute

// SyncResetHistoryFromUpstream 拉取上游 reset/status 的 latest_*_reset_history，
// 把非本网关执行的重置（官方客户端、其他设备等）补记到 claim_records，
// 使重置历史与真实一致。used_at 为毫秒时间戳；
// 去重：本地 ±15 分钟内已有成功重置记录则不重复入库；锚点存 settings。
func (z *ZCodeAPI) SyncResetHistoryFromUpstream(a *Account) (int, error) {
	if a == nil || a.ZCodeJWT == "" {
		return 0, nil
	}
	z.resetSyncMu.Lock()
	if last, ok := z.resetSyncAt[a.ID]; ok && time.Since(last) < resetHistorySyncInterval {
		z.resetSyncMu.Unlock()
		return 0, nil
	}
	z.resetSyncAt[a.ID] = time.Now()
	z.resetSyncMu.Unlock()

	st, _, _, err := z.FetchResetStatus(a)
	if err != nil {
		return 0, err
	}
	type slot struct {
		kind string
		used int64 // 毫秒
	}
	var latest []slot
	if st.LatestFiveHourReset != nil && st.LatestFiveHourReset.UsedAt > 0 {
		latest = append(latest, slot{"FIVE_HOUR", st.LatestFiveHourReset.UsedAt})
	}
	if st.LatestWeekReset != nil && st.LatestWeekReset.UsedAt > 0 {
		latest = append(latest, slot{"WEEK", st.LatestWeekReset.UsedAt})
	}

	anchorKey := fmt.Sprintf("reset_history_seen:%d", a.ID)
	anchor := map[string]int64{}
	if raw, err := z.db.GetSetting(anchorKey); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &anchor)
	}

	// 与 UseReset 本地落记录互斥（同一把账号级锁）：关闭"上游已接受重置、
	// 本地尚未落记录"亚秒窗口内同步读到新 used_at 造成重复入库的竞态
	mu := z.claimLockFor(a.ID)
	mu.Lock()
	defer mu.Unlock()

	inserted := 0
	changed := false
	for _, e := range latest {
		if e.used/1000 <= anchor[e.kind]/1000 {
			continue
		}
		dup, err := z.db.HasResetRecordNear(a.ID, e.used/1000, e.kind)
		if err != nil {
			continue // 查询失败不推进锚点，下次同步重试
		}
		if !dup {
			if err := z.db.InsertClaimRecord(&ClaimRecord{
				AccountID: a.ID,
				Email:     a.Email,
				TaskType:  "reset",
				PlanName:  "配额重置(" + e.kind + ")",
				Success:   true,
				Message:   "上游重置记录（非本网关执行）",
			}); err != nil {
				// 入库失败不推进锚点：下轮同步重试，否则该记录永久丢失
				log.Printf("[reset] insert upstream reset record %s account=%s: %v", e.kind, a.Email, err)
				continue
			}
			inserted++
		}
		anchor[e.kind] = e.used
		changed = true
	}
	if changed {
		if raw, err := json.Marshal(anchor); err == nil {
			z.db.SetSetting(anchorKey, string(raw))
		}
	}
	return inserted, nil
}
