package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---- 管理 REST API ----

// APIServer 管理接口服务
type APIServer struct {
	db        *DB
	cfg       *FileConfig
	pool      *AccountPool
	zapi      *ZCodeAPI
	oauth     *OAuthManager
	acctMgr   *AccountManager
	scheduler *CronScheduler
	auth      *AuthManager
	captcha   *CaptchaService
}

// NewAPIServer 创建 API 服务
func NewAPIServer(db *DB, cfg *FileConfig, pool *AccountPool, zapi *ZCodeAPI,
	oauth *OAuthManager, acctMgr *AccountManager, scheduler *CronScheduler,
	auth *AuthManager, captcha *CaptchaService) *APIServer {
	return &APIServer{db: db, cfg: cfg, pool: pool, zapi: zapi, oauth: oauth,
		acctMgr: acctMgr, scheduler: scheduler, auth: auth, captcha: captcha}
}

// RegisterRoutes 注册路由（Go 1.22+ 方法模式）
func (s *APIServer) RegisterRoutes(mux *http.ServeMux) {
	// 认证
	mux.HandleFunc("POST /api/login", s.auth.HandleLogin)
	mux.HandleFunc("POST /api/logout", s.auth.HandleLogout)
	mux.HandleFunc("GET /api/auth/check", s.auth.HandleCheckAuth)
	mux.HandleFunc("POST /api/auth/password", s.auth.HandleChangePassword)
	mux.HandleFunc("GET /api/settings/api-key", s.auth.HandleGetAPIKey)
	mux.HandleFunc("POST /api/settings/api-key/generate", s.auth.HandleGenerateAPIKey)

	// 仪表盘
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)

	// 账号
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts/import/local", s.handleImportLocal)
	mux.HandleFunc("POST /api/accounts/import/paste", s.handleImportPaste)
	mux.HandleFunc("POST /api/accounts/import/bundle", s.handleImportBundle)
	mux.HandleFunc("POST /api/accounts/export", s.handleExportBundle)
	mux.HandleFunc("POST /api/accounts/oauth/start", s.handleOAuthStart)
	mux.HandleFunc("POST /api/accounts/oauth/manual", s.handleOAuthManual)
	mux.HandleFunc("GET /api/accounts/oauth/status", s.handleOAuthStatus)
	mux.HandleFunc("GET /api/offpeak/availability", s.handleOffpeakAvailability)
	mux.HandleFunc("POST /api/accounts/{id}/refresh", s.handleAccountRefresh)
	mux.HandleFunc("POST /api/accounts/{id}/claim", s.handleAccountClaim)
	mux.HandleFunc("POST /api/accounts/{id}/detect", s.handleAccountDetect)
	mux.HandleFunc("POST /api/accounts/{id}/activate", s.handleAccountActivate)
	mux.HandleFunc("POST /api/accounts/{id}/reset", s.handleAccountReset)
	mux.HandleFunc("GET /api/accounts/{id}/reset-status", s.handleResetStatus)
	mux.HandleFunc("POST /api/accounts/{id}/switch-back", s.handleSwitchBack)
	mux.HandleFunc("POST /api/accounts/{id}/restore-local", s.handleRestoreLocal)
	mux.HandleFunc("PUT /api/accounts/{id}", s.handleUpdateAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("GET /api/groups", s.handleListGroups)

	// 活动计划
	mux.HandleFunc("GET /api/plans", s.handleListPlans)
	mux.HandleFunc("POST /api/plans", s.handleSavePlan)
	mux.HandleFunc("PUT /api/plans/{id}", s.handleSavePlan)
	mux.HandleFunc("DELETE /api/plans/{id}", s.handleDeletePlan)
	mux.HandleFunc("POST /api/plans/{id}/run", s.handleRunPlan)
	mux.HandleFunc("GET /api/plans/running", s.handleRunningPlans)
	mux.HandleFunc("GET /api/plan-runs", s.handlePlanRuns)

	// 记录
	mux.HandleFunc("GET /api/claim-records", s.handleClaimRecords)
	mux.HandleFunc("GET /api/usage-records", s.handleUsageRecords)
	mux.HandleFunc("GET /api/stats", s.handleStats)

	// 下游网关 Key（R1）
	mux.HandleFunc("GET /api/keys", s.handleListKeys)
	mux.HandleFunc("POST /api/keys", s.handleCreateKey)
	mux.HandleFunc("PUT /api/keys/{id}", s.handleUpdateKey)
	mux.HandleFunc("DELETE /api/keys/{id}", s.handleDeleteKey)

	// 设置
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)

	// 代理
	mux.HandleFunc("GET /api/proxies", s.handleListProxies)
	mux.HandleFunc("POST /api/proxies", s.handleSaveProxy)
	mux.HandleFunc("PUT /api/proxies/{id}", s.handleSaveProxy)
	mux.HandleFunc("DELETE /api/proxies/{id}", s.handleDeleteProxy)
	mux.HandleFunc("POST /api/proxies/{id}/test", s.handleTestProxy)
	mux.HandleFunc("POST /api/proxies/test-url", s.handleTestProxyURL)
	mux.HandleFunc("GET /api/proxies/system", s.handleSystemProxy)
	mux.HandleFunc("GET /api/proxies/probe-ports", s.handleProbePorts)

	// 验证码
	mux.HandleFunc("GET /api/captcha/status", s.handleCaptchaStatus)
	mux.HandleFunc("POST /api/captcha/invalidate", s.handleCaptchaInvalidate)
	mux.HandleFunc("POST /api/captcha/solve", s.handleCaptchaSolve)

	// 指纹
	mux.HandleFunc("GET /api/fingerprints", s.handleFingerprints)

	// 模型
	mux.HandleFunc("GET /api/models", s.handleModelList)
	mux.HandleFunc("POST /api/models/sync", s.handleModelSync)
	mux.HandleFunc("GET /api/models/catalog", s.handleModelCatalog)
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// ---- 仪表盘 ----

func (s *APIServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	accounts, _ := s.db.ListAccounts("")
	statusCount := map[string]int{}
	groupCount := map[string]int{}
	totalRemaining := 0.0
	for _, a := range accounts {
		st := EffectiveStatus(a)
		statusCount[st]++
		g := a.AccountGroup
		if g == "" {
			g = "未分组"
		}
		groupCount[g]++
		totalRemaining += a.Remaining
	}
	stats, _ := s.db.UsageStats(7)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_total":      len(accounts),
		"account_selectable": s.pool.SelectableCount(""),
		"status_count":       statusCount,
		"group_count":        groupCount,
		"total_remaining":    totalRemaining,
		"usage_7d":           stats,
		"app_version":        s.zapi.appVersion,
		"captcha":            s.captcha.Status(),
	})
}

// ---- 账号 ----

// accountPublicView 脱敏账号视图
func accountPublicView(a *Account) map[string]interface{} {
	mask := func(tok string) string {
		if tok == "" {
			return ""
		}
		// 任何长度都脱敏，短 token 也不全量返回
		if len(tok) <= 8 {
			return "****"
		}
		return tok[:4] + "…" + tok[len(tok)-4:]
	}
	var quota *QuotaOverview
	if a.QuotaJSON != "" {
		var ov QuotaOverview
		if json.Unmarshal([]byte(a.QuotaJSON), &ov) == nil {
			quota = &ov
		}
	}
	return map[string]interface{}{
		"id": a.ID, "user_id": a.UserID, "email": a.Email, "display_name": a.DisplayName,
		"provider": a.Provider, "auth_type": a.AuthType,
		"jwt_masked": mask(a.ZCodeJWT), "api_key_masked": mask(a.APIKey),
		"has_jwt": a.ZCodeJWT != "", "has_api_key": a.APIKey != "",
		"has_access_token": a.AccessToken != "", "has_creds_snapshot": a.CredsRaw != "",
		"device_mid": a.DeviceMid,
		"status":     EffectiveStatus(a), "raw_status": a.Status, "enabled": a.Enabled,
		"group": a.AccountGroup, "remark": a.Remark, "priority": accountPriority(a),
		"plan_tier": a.PlanTier, "plan_expire": a.PlanExpire,
		"total_units": a.TotalUnits, "used_units": a.UsedUnits, "remaining": a.Remaining,
		"quota":              quota,
		"use_count":          a.UseCount,
		"fail_count":         a.FailCount,
		"last_used_at":       a.LastUsedAt,
		"last_checked_at":    a.LastCheckedAt,
		"cooling_until":      a.CoolingUntil,
		"last_error":         a.LastError,
		"paid_fallback":      a.PaidFallback,
		"paid_cooling_until": a.PaidCoolingUntil,
		"last_claim_at":      a.LastClaimAt, "last_claim_plan": a.LastClaimPlan, "last_claim_msg": a.LastClaimMsg,
		"created_at": a.CreatedAt, "updated_at": a.UpdatedAt,
	}
}

func (s *APIServer) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	group := r.URL.Query().Get("group")
	accounts, err := s.db.ListAccounts("")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 组过滤走 matchAccountGroup：account_group 允许逗号分隔多组，SQL 精确等值
	// 会漏掉 "eu,failover" 这类账号（转发/计划/闲时选择均按该语义）
	if group != "" {
		filtered := make([]*Account, 0, len(accounts))
		for _, a := range accounts {
			if matchAccountGroup(a, group) {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}
	out := make([]map[string]interface{}, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, accountPublicView(a))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"accounts": out, "total": len(out)})
}

func (s *APIServer) handleImportLocal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Group string `json:"group"`
	}
	// body 可选（group 默认空），但必须是合法 JSON：截断/畸形请求不得静默按空值导入
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	a, err := s.acctMgr.ImportFromLocalClient(body.Group)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "account": accountPublicView(a)})
}

func (s *APIServer) handleImportPaste(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
		Secret   string `json:"secret"`
		Group    string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	a, err := s.acctMgr.ImportPasted(body.Provider, body.Name, body.Secret, body.Group)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "account": accountPublicView(a)})
}

// handleExportBundle 导出加密账号包（步进重认证：需再次提供当前管理口令）
func (s *APIServer) handleExportBundle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password      string  `json:"password"`
		AdminPassword string  `json:"admin_password"`
		IDs           []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// 导出会带走全部凭据。独立限速键 ip|export（与登录的 ip|user 分开计数），
	// 同样走指数退避：stolen session 无法无节流爆破管理员口令
	rateKey := clientIP(r) + "|export"
	if wait := s.auth.checkLoginRate(rateKey); wait > 0 {
		writeAPIError(w, http.StatusTooManyRequests,
			fmt.Sprintf("尝试过于频繁，请 %d 秒后再试", int(wait.Seconds())+1))
		return
	}
	if !s.auth.verifyPassword(body.AdminPassword) {
		s.auth.recordLoginFail(rateKey)
		writeAPIError(w, http.StatusUnauthorized, "管理员密码验证失败，请输入当前管理员密码")
		return
	}
	s.auth.clearLoginFail(rateKey)
	bundle, err := s.acctMgr.ExportBundle(body.Password, body.IDs)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"bundle": bundle})
}

// handleImportBundle 导入加密账号包
func (s *APIServer) handleImportBundle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
		Bundle   string `json:"bundle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	count, err := s.acctMgr.ImportBundle(body.Password, body.Bundle)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "imported": count})
}

func (s *APIServer) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Manual bool   `json:"manual"`
		Poll   bool   `json:"poll"` // 服务端中介轮询登录（免回调，免注册 redirect_uri）
		Group  string `json:"group"`
	}
	// body 可选，但畸形 JSON 会把 poll 请求静默变成回调流程——必须报错
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Poll {
		flow, authURL, err := s.oauth.StartPollLogin(body.Group)
		if err != nil {
			writeAPIError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"state":         flow.State,
			"authorize_url": authURL,
			"poll":          true,
		})
		return
	}
	flow, authURL := s.oauth.StartLogin(body.Manual, body.Group)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"state":         flow.State,
		"authorize_url": authURL,
		"manual":        body.Manual,
	})
}

func (s *APIServer) handleOAuthManual(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
		Input string `json:"input"` // 回跳 URL 或 code
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.oauth.SubmitManual(body.State, body.Input); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "已提交，正在后台兑换"})
}

func (s *APIServer) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	flow := s.oauth.FlowStatus(state)
	if flow == nil {
		writeAPIError(w, http.StatusNotFound, "流程不存在或已过期")
		return
	}
	writeJSON(w, http.StatusOK, flow)
}

// handleOffpeakAvailability 探闲时队列可用性（管理端，取第一个可用 JWT 账号）
func (s *APIServer) handleOffpeakAvailability(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.db.ListAccounts("")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tickets := offPeakTickets{z: s.zapi}
	lastErr := ""
	for _, a := range accounts {
		if !a.Enabled || a.ZCodeJWT == "" || a.Provider != "zai" {
			continue
		}
		canTake, nextTakeAt, err := tickets.Availability(r.Context(), a)
		if err != nil {
			// 单个账号（如 JWT 过期）不应让整个队列显示不可用：继续探测下一个
			lastErr = a.DisplayNameOrEmail() + ": " + err.Error()
			continue
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok": true, "can_take": canTake, "next_take_at": nextTakeAt, "account": a.DisplayNameOrEmail(),
		})
		return
	}
	if lastErr != "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "error": "所有账号探测失败（" + truncate(lastErr, 300) + "）"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "error": "无可用 JWT 账号"})
}

func (s *APIServer) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.zapi.RefreshAccountQuota(a); err != nil {
		if errors.Is(err, errRefreshInFlight) {
			// 已有并发刷新在跑：不算失败，返回当前快照即可（账号可能已被并发删除）
			a2, err2 := s.db.GetAccount(id)
			if err2 != nil {
				writeAPIError(w, http.StatusNotFound, err2.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "account": accountPublicView(a2), "message": "刷新已在进行中"})
			return
		}
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	a2, err := s.db.GetAccount(id)
	if err != nil {
		// 刷新期间账号被删：如实 404，不得拿 nil 账号渲染
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "account": accountPublicView(a2)})
}

func (s *APIServer) handleAccountClaim(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	result := s.zapi.ClaimForAccount(a)
	writeJSON(w, http.StatusOK, result)
}

func (s *APIServer) handleAccountDetect(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	plans, err := s.zapi.PreviewPlans(a)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"plans": plans, "total": len(plans)})
}

func (s *APIServer) handleAccountActivate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	result := s.zapi.ActivateForAccount(a)
	writeJSON(w, http.StatusOK, result)
}

// handleAccountReset 执行 Coding Plan 配额重置（审计发现接口）
func (s *APIServer) handleAccountReset(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	result := s.zapi.ResetForAccount(a)
	writeJSON(w, http.StatusOK, result)
}

// handleResetStatus 查询重置机会（five_hour / week）
func (s *APIServer) handleResetStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	st, httpStatus, bizMsg, err := s.zapi.FetchResetStatus(a)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "message": firstNonEmpty(bizMsg, err.Error()), "http_status": httpStatus})
		return
	}
	synced, syncErr := s.zapi.SyncResetHistoryFromUpstream(a)
	resp := map[string]interface{}{"ok": true, "status": st, "upstream_synced": synced}
	if syncErr != nil {
		resp["sync_message"] = syncErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleModelSync 同步官方模型目录
func (s *APIServer) handleModelSync(w http.ResponseWriter, r *http.Request) {
	models, err := s.zapi.SyncModelCatalog()
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "models": models, "total": len(models)})
}

// handleModelCatalog 读取缓存的模型目录
func (s *APIServer) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": s.zapi.GetModelCatalog()})
}

func (s *APIServer) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Group        *string `json:"group"`
		Remark       *string `json:"remark"`
		Enabled      *bool   `json:"enabled"`
		Priority     *int64  `json:"priority"`
		PaidFallback *bool   `json:"paid_fallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	a, err := s.db.GetAccount(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	group, remark, enabled, paidFallback := a.AccountGroup, a.Remark, a.Enabled, a.PaidFallback
	if body.Group != nil {
		group = *body.Group
	}
	if body.Remark != nil {
		remark = *body.Remark
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if body.PaidFallback != nil {
		paidFallback = *body.PaidFallback
	}
	// 全部入参先校验再动笔：否则 priority 非法时 400，但 group/remark/enabled
	// 已落库——面板以为保存失败，重试时静默保留了上一次"失败"的修改。
	// 0 不是有效值（accountPriority 会把 0 归一化回默认值，API 却显示保存成功）
	if body.Priority != nil && (*body.Priority < 1 || *body.Priority > 9999) {
		writeAPIError(w, http.StatusBadRequest, "priority 取值范围 1-9999")
		return
	}
	// 单事务更新（字段+priority 原子落库）：两条独立 UPDATE 会在第二条失败时
	// 留下"半保存"状态，面板以为全失败而前一半已生效
	if err := s.db.UpdateAccountFieldsWithPriority(id, group, remark, enabled, paidFallback, body.Priority); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *APIServer) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.db.DeleteAccount(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *APIServer) handleSwitchBack(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		KillClient bool `json:"kill_client"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.acctMgr.SwitchBackToLocal(id, body.KillClient); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "已写回本地 ZCode 客户端，重启客户端生效",
	})
}

func (s *APIServer) handleRestoreLocal(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.acctMgr.RestoreLocalFromSnapshot(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "已从快照还原本地客户端"})
}

func (s *APIServer) handleListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.db.ListGroups()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if groups == nil {
		groups = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"groups": groups})
}

// ---- 活动计划 ----

func (s *APIServer) handleListPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.db.ListClaimPlans()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(plans))
	now := time.Now()
	for _, p := range plans {
		item := map[string]interface{}{
			"id": p.ID, "plan_name": p.PlanName, "cron_expr": p.CronExpr,
			"is_active": p.IsActive, "target_type": p.TargetType, "account_id": p.AccountID,
			"account_group": p.AccountGroup, "task_type": p.TaskType, "auto_pick": p.AutoPick,
			"delay_seconds": p.DelaySeconds, "last_run_at": p.LastRunAt,
			"last_run_status": p.LastRunStatus, "last_run_msg": p.LastRunMsg,
			"created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
		}
		if next := NextRunTime(p.CronExpr, now); !next.IsZero() {
			item["next_run_at"] = next.Format("2006-01-02 15:04")
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"plans": out})
}

func (s *APIServer) handleSavePlan(w http.ResponseWriter, r *http.Request) {
	var p ClaimPlan
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if idStr := r.PathValue("id"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid id")
			return
		}
		p.ID = id
	}
	if err := ValidateCronExpr(p.CronExpr); err != nil {
		writeAPIError(w, http.StatusBadRequest, "cron 表达式无效: "+err.Error())
		return
	}
	if p.TaskType == "" {
		p.TaskType = "claim"
	}
	switch p.TaskType {
	case "claim", "detect", "activate", "reset":
	default:
		writeAPIError(w, http.StatusBadRequest, "无效任务类型: "+p.TaskType)
		return
	}
	if p.TargetType == "" {
		p.TargetType = "all_accounts"
	}
	switch p.TargetType {
	case "all_accounts", "single_account", "group":
	default:
		// 未知 target_type 会被调度器当 all_accounts 处理——写错一个字母
		// 就从"单账号"静默变成"全账号"，必须保存时就拒绝
		writeAPIError(w, http.StatusBadRequest, "无效目标类型: "+p.TargetType)
		return
	}
	if p.TargetType == "single_account" && p.AccountID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "single_account 目标必须提供有效 account_id")
		return
	}
	// POST 路径不含 {id}：客户端误带 body id 会把"新建"静默变成"覆盖他者"
	if r.Method == http.MethodPost {
		p.ID = 0
	} else if _, err := s.db.GetClaimPlan(p.ID); err != nil {
		// PUT：目标不存在时如实 404，而不是 0 行更新的假成功
		writeAPIError(w, http.StatusNotFound, "计划不存在: "+strconv.FormatInt(p.ID, 10))
		return
	}
	id, err := s.db.SaveClaimPlan(&p)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "id": id})
}

func (s *APIServer) handleDeletePlan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.db.DeleteClaimPlan(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *APIServer) handleRunPlan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.scheduler.RunPlanNow(id); err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "不存在"):
			writeAPIError(w, http.StatusNotFound, msg)
		case strings.Contains(msg, "正在执行"):
			writeAPIError(w, http.StatusConflict, msg)
		default:
			writeAPIError(w, http.StatusBadRequest, msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "计划已开始执行"})
}

func (s *APIServer) handleRunningPlans(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"running": s.scheduler.GetRunning()})
}

func (s *APIServer) handlePlanRuns(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	records, err := s.db.ListPlanRunRecords(limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"records": records})
}

// ---- 记录与统计 ----

func (s *APIServer) handleClaimRecords(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100)
	// account_id 不传 = 不过滤；显式传了就必须是正整数，否则 400
	// （静默吞掉会让脚本消费者在不知情下拿到全账号数据）
	accountID := int64(0)
	if v := r.URL.Query().Get("account_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeAPIError(w, http.StatusBadRequest, "account_id 须为正整数")
			return
		}
		accountID = n
	}
	records, err := s.db.ListClaimRecords(limit, accountID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"records": records})
}

func (s *APIServer) handleUsageRecords(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100)
	records, err := s.db.ListUsageRecords(limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"records": records})
}

func (s *APIServer) handleStats(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 7)
	stats, err := s.db.UsageStats(days)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 当日付费通道 token 消耗（免费优先/付费回退策略的观测口径）
	if paid, err := s.db.PaidTokensToday(); err == nil {
		stats["paid_tokens_today"] = paid
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---- 设置 ----

// settingsWhitelist 允许前端修改的设置键
var settingsWhitelist = map[string]bool{
	"selection_strategy": true, "quota_refresh_interval": true,
	"upstream_proxy": true, "fingerprint": true, "custom_ja3": true,
	"captcha_mode": true, "gateway_models": true, "sticky_sessions": true,
	"prompt_cache_breakpoint": true,
	"captcha_prewarm":         true,
	"auto_claim_promos":       true, "auto_claim_interval_minutes": true, "auto_claim_delay_seconds": true,
	"auto_reset_enabled": true, "auto_reset_min_wait_minutes": true, "auto_reset_min_wait_week_hours": true,
	"auto_reset_expiry_enabled": true, "auto_reset_expiry_spend_minutes": true,
	"max_concurrent_per_account": true,
	// 免费优先 / 付费回退策略
	"paid_fallback_mode": true, "paid_daily_token_cap": true,
	// 闲时免费通道（off-peak ticket queue）
	"async_enabled": true, "async_poll_interval_ms": true,
	"async_keepalive_ms": true, "async_max_retries": true, "async_max_wait_sec": true,
	// 用量记录保留天数（0=永久；每 6h 清扫一次）
	"usage_retention_days": true,
}

func (s *APIServer) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := s.db.AllSettings()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 敏感项脱敏
	delete(all, "password_hash")
	if k, ok := all["api_key"]; ok && k != "" {
		all["has_api_key"] = "1"
		delete(all, "api_key")
	}
	all["app_version"] = s.zapi.appVersion
	all["listen_addr"] = s.cfg.GetListenAddr()
	writeJSON(w, http.StatusOK, all)
}

func (s *APIServer) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated := []string{}
	// 两阶段：先整体校验再落库。map 迭代顺序随机，边验边写会在批次含非法值时
	// 非确定性地写入一半设置却返回 400
	type settingKV struct{ k, v string }
	pending := []settingKV{}
	for k, v := range body {
		if !settingsWhitelist[k] {
			continue
		}
		if k == "selection_strategy" {
			switch v {
			case StrategyRandom, StrategyRoundRobin, StrategyBestQuota, StrategyPriority:
			default:
				writeAPIError(w, http.StatusBadRequest, "无效策略: "+v)
				return
			}
		}
		if k == "quota_refresh_interval" {
			if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 86400 {
				writeAPIError(w, http.StatusBadRequest, "无效刷新间隔")
				return
			}
		}
		// 用量保留天数：0=永久，上限 10 年（防手滑多零把清理变摆设）
		if k == "usage_retention_days" {
			if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 3650 {
				writeAPIError(w, http.StatusBadRequest, "无效保留天数（0=永久，上限 3650）")
				return
			}
		}
		// auto-reset 数值项按 UI 界限校验：静默收下非法值会让 settingInt 回落
		// 默认（保存≠生效），或把"临期窗口"撑到 10 天令每个槽位恒"临期"
		if k == "auto_reset_min_wait_minutes" || k == "auto_reset_expiry_spend_minutes" {
			if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 1440 {
				writeAPIError(w, http.StatusBadRequest, "无效阈值（0-1440 分钟）")
				return
			}
		}
		if k == "auto_reset_min_wait_week_hours" {
			if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 168 {
				writeAPIError(w, http.StatusBadRequest, "无效阈值（0-168 小时）")
				return
			}
		}
		// 付费回退策略：白名单取值；token 上限非负整数（0=不限）
		if k == "paid_fallback_mode" {
			switch v {
			case PaidModeFreeFirst, PaidModeBalanced, PaidModeNever:
			default:
				writeAPIError(w, http.StatusBadRequest, "无效付费回退策略: "+v)
				return
			}
		}
		if k == "paid_daily_token_cap" {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err != nil || n < 0 {
				writeAPIError(w, http.StatusBadRequest, "无效付费 token 上限（非负整数，0=不限）")
				return
			}
		}
		// 指纹设置服务端校验：非法值会导致所有上游连接失败
		if k == "fingerprint" && !isValidFingerprint(v) {
			writeAPIError(w, http.StatusBadRequest, "无效指纹预设: "+v)
			return
		}
		if k == "custom_ja3" && strings.TrimSpace(v) != "" {
			if _, err := ja3ToClientHelloSpec(v); err != nil {
				writeAPIError(w, http.StatusBadRequest, "无效 JA3: "+err.Error())
				return
			}
		}
		if k == "upstream_proxy" && strings.TrimSpace(v) != "" {
			// 无校验的代理值会静默退化成直连（隐私失败）或全量连接失败
			trimmed := strings.TrimSpace(v)
			u, err := url.Parse(trimmed)
			if err != nil || u.Host == "" {
				writeAPIError(w, http.StatusBadRequest, "无效上游代理地址（示例: http://host:port 或 socks5://user:pass@host:port）")
				return
			}
			switch u.Scheme {
			case "http", "https", "socks5", "socks5h":
			default:
				writeAPIError(w, http.StatusBadRequest, "不支持的代理协议: "+u.Scheme+"（支持 http/https/socks5/socks5h）")
				return
			}
			if (u.Scheme == "socks5" || u.Scheme == "socks5h") && u.Port() == "" {
				writeAPIError(w, http.StatusBadRequest, "socks5 代理必须带端口（如 socks5://host:1080）")
				return
			}
			v = trimmed // 校验通过后存 trimmed 值，保证"所存即可解析"
		}
		pending = append(pending, settingKV{k, v})
	}
	for _, kv := range pending {
		if err := s.db.SetSetting(kv.k, kv.v); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		updated = append(updated, kv.k)
	}
	// 指纹/代理变更：失效并关闭缓存的上游客户端连接池
	for _, k := range updated {
		if k == "fingerprint" || k == "custom_ja3" || k == "upstream_proxy" {
			CloseIdleClients()
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "updated": updated})
}

// ---- 代理 ----

func (s *APIServer) handleListProxies(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.db.ListProxyNodes()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		item := map[string]interface{}{
			"id": n.ID, "name": n.Name, "type": n.Type, "host": n.Host, "port": n.Port,
			"username": n.Username, "has_password": n.Password != "",
			"password_broken": n.PasswordBroken,
			"is_default":      n.IsDefault, "group_name": n.GroupName, "enabled": n.Enabled,
			"check_status": n.CheckStatus, "check_latency": n.CheckLatency,
			"check_ip": n.CheckIP, "check_msg": n.CheckMsg, "check_at": n.CheckAt,
			"created_at": n.CreatedAt, "updated_at": n.UpdatedAt,
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"proxies": out})
}

func (s *APIServer) handleSaveProxy(w http.ResponseWriter, r *http.Request) {
	var n ProxyNode
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if idStr := r.PathValue("id"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid id")
			return
		}
		n.ID = id
	}
	if n.Host == "" || n.Port <= 0 || n.Port > 65535 {
		writeAPIError(w, http.StatusBadRequest, "host 必填，port 须在 1-65535")
		return
	}
	switch n.Type {
	case "socks5", "http", "https":
	default:
		writeAPIError(w, http.StatusBadRequest, "无效代理类型: "+n.Type+"（支持 socks5/http/https）")
		return
	}
	if r.Method == http.MethodPost {
		n.ID = 0 // POST 不接受 body id：防止"新建"静默覆盖既有节点
	} else {
		exists := false
		if nodes, err := s.db.ListProxyNodes(); err == nil {
			for _, o := range nodes {
				if o.ID == n.ID {
					exists = true
					break
				}
			}
		}
		if !exists {
			writeAPIError(w, http.StatusNotFound, "代理节点不存在")
			return
		}
	}
	// 空密码 = 保持原密码：由 SaveProxyNode 直接跳过密码列实现（读回明文
	// 再回写会在钥匙缺失期把解不开的密文冲成 ''，永久销毁）
	id, err := s.db.SaveProxyNode(&n)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "id": id})
}

func (s *APIServer) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.db.DeleteProxyNode(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *APIServer) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	nodes, _ := s.db.ListProxyNodes()
	var target *ProxyNode
	for _, n := range nodes {
		if n.ID == id {
			target = n
			break
		}
	}
	if target == nil {
		writeAPIError(w, http.StatusNotFound, "代理节点不存在")
		return
	}
	proxyURL := ProxyURLForNode(target)
	ip, elapsed, err := TestProxyExitIP(proxyURL)
	status, msg := "ok", ""
	if err != nil {
		status, msg = "fail", err.Error()
	}
	s.db.UpdateProxyNodeCheck(id, status, int(elapsed.Milliseconds()), ip, msg)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": err == nil, "exit_ip": ip, "elapsed_ms": elapsed.Milliseconds(),
		"message": msg, "proxy": MaskProxyURL(proxyURL),
	})
}

func (s *APIServer) handleTestProxyURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ip, elapsed, err := TestProxyExitIP(strings.TrimSpace(body.URL))
	resp := map[string]interface{}{
		"ok": err == nil, "exit_ip": ip, "elapsed_ms": elapsed.Milliseconds(),
	}
	if err != nil {
		resp["message"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *APIServer) handleSystemProxy(w http.ResponseWriter, r *http.Request) {
	enabled, proxyURL := DetectSystemProxy()
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": enabled, "url": proxyURL})
}

func (s *APIServer) handleProbePorts(w http.ResponseWriter, r *http.Request) {
	ports := ProbeLocalProxyPorts()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ports": ports})
}

// ---- 验证码 ----

func (s *APIServer) handleCaptchaStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.captcha.Status())
}

func (s *APIServer) handleCaptchaInvalidate(w http.ResponseWriter, r *http.Request) {
	s.captcha.Invalidate()
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// handleCaptchaSolve 手动触发一次求解（调试用；有头模式下会弹浏览器窗口）
func (s *APIServer) handleCaptchaSolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID int64 `json:"account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var a *Account
	if body.AccountID > 0 {
		var err error
		a, err = s.db.GetAccount(body.AccountID)
		// 静默降级到全局代理会在错误的出口 IP 下解题并缓存，后续该组账号
		// 重放必然触发风控：显式 404 而不是 nil 账号继续
		if err != nil || a == nil {
			writeAPIError(w, http.StatusNotFound, "account not found")
			return
		}
	}
	param, region, err := s.captcha.GetVerifyParamCtx(r.Context(), a)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": param != "", "param_len": len(param), "region": region,
	})
}

// ---- 指纹 ----

func (s *APIServer) handleFingerprints(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]string, 0, len(tlsFingerprintPresets))
	for _, p := range tlsFingerprintPresets {
		out = append(out, map[string]string{"id": p.ID, "label": p.Label, "group": p.Group})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"fingerprints": out})
}

// ---- 模型 ----

func (s *APIServer) handleModelList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": s.cfg.GetModels()})
}

// ---- 工具 ----

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	// 夹取范围，防止 LIMIT -1 / 超大值拉全表
	if n < 1 {
		return 1
	}
	if n > 500 {
		return 500
	}
	return n
}
