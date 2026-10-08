package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---- ZCode 上游 API 封装 ----
// 持有账号池 / 验证码服务 / 出口代理，提供额度刷新、活动领取、聊天转发。

type ZCodeAPI struct {
	cfg        *FileConfig
	db         *DB
	pool       *AccountPool
	captcha    *CaptchaService
	egress     *EgressProxy
	routing    *EndpointRouter
	appVersion string

	claimMu    sync.Mutex
	claimLocks map[int64]*sync.Mutex // 账号级领取互斥（防 UI+cron 并发双领）

	resetSyncMu sync.Mutex
	resetSyncAt map[int64]time.Time // 上游重置历史同步节流（每账号）

	quotaRefreshInflight sync.Map // 账号ID → 刷新中（单飞，防并发请求对 billing 形成风暴）

	asyncRotation atomic.Int64 // 闲时通道轮转起点（并发请求分摊账号）

	tokenRefreshInflight sync.Map // 账号ID → refresh_token 兑换中（单飞）
}

// NewZCodeAPI 创建上游 API 封装，并把额度刷新函数注入账号池
func NewZCodeAPI(cfg *FileConfig, db *DB, pool *AccountPool, captcha *CaptchaService, appVersion string) *ZCodeAPI {
	z := &ZCodeAPI{
		cfg:         cfg,
		db:          db,
		pool:        pool,
		captcha:     captcha,
		egress:      NewEgressProxy(db),
		routing:     NewEndpointRouter(""),
		appVersion:  appVersion,
		claimLocks:  make(map[int64]*sync.Mutex),
		resetSyncAt: make(map[int64]time.Time),
	}
	pool.SetQuotaFetcher(func(a *Account) error {
		err := z.RefreshAccountQuota(a)
		if errors.Is(err, errRefreshInFlight) {
			// 并发刷新已被别的副本赢下：本周期跳过，不算失败也不烧重置同步
			return nil
		}
		if n, serr := z.SyncResetHistoryFromUpstream(a); serr != nil {
			log.Printf("[reset] 上游重置历史同步失败 account=%s: %v", a.Email, serr)
		} else if n > 0 {
			log.Printf("[reset] 同步到 %d 条上游重置记录 account=%s", n, a.Email)
		}
		return err
	})
	return z
}

// errRefreshInFlight 已有并发刷新在跑（调用方可视为"数据正在更新"而非失败）
var errRefreshInFlight = errors.New("quota refresh already in flight")

// RefreshAccountQuota 拉取额度 → 应用状态迁移 → 落库。
// 进程内单飞：并发触发（relay 成功后节流刷新 + 池子周期刷新）同一账号只拉一次。
func (z *ZCodeAPI) RefreshAccountQuota(a *Account) error {
	if _, busy := z.quotaRefreshInflight.LoadOrStore(a.ID, struct{}{}); busy {
		return errRefreshInFlight
	}
	defer z.quotaRefreshInflight.Delete(a.ID)
	ov, err := z.FetchQuotaRaw(a)
	if err != nil {
		return err
	}
	z.applyQuotaResult(a, ov)
	return nil
}

// applyQuotaResult 根据额度结果驱动状态机（quota.py 状态迁移移植）
func (z *ZCodeAPI) applyQuotaResult(a *Account, ov *QuotaOverview) {
	switch {
	case ov.AuthFailed:
		// 凭证失效：先尝试 refresh_token 兑换新 JWT，成功（或已有并发刷新
		// 在跑）则不判死，等下一轮用新凭证确认
		if ok, inflight := z.tryRefreshAccount(a); ok || inflight {
			return
		}
		if z.credentialsAlreadyRotated(a) {
			return
		}
		z.pool.MarkInvalid(a, "额度接口鉴权失败（401/403），凭证可能已过期")
		return
	case ov.NotEntitled:
		z.pool.MarkInactive(a, "Coding Plan 未激活（不存在订阅资格）")
		return
	case ov.AllExhausted():
		z.pool.MarkExhausted(a, "额度已用完")
	default:
		// 有剩余额度：cooling 到期 / exhausted / inactive / invalid（凭证其实有效）恢复 active
		if a.tryRecoverActive() {
			z.db.SetAccountStatus(a.ID, StatusActive, "", 0)
			log.Printf("[quota] account %s recovered -> active", a.Email)
		}
	}

	// F5：免费促销档（Start / 体验 / trial）自动降 priority，priority 策略下
	// 促销账号先于付费账号被消费。仅首次（UseCount==0 且仍为默认值）生效，
	// 不覆盖用户在账号编辑里的手动调整。
	if ov.PlanTier != "" && a.UseCount == 0 && accountPriority(a) == DefaultPriority && isPromoTier(ov.PlanTier) {
		a.Priority = PromoPriority
		if err := z.db.UpdateAccountPriority(a.ID, PromoPriority); err != nil {
			log.Printf("[quota] auto promo priority %s: %v", a.Email, err)
		} else {
			log.Printf("[quota] account %s: promo tier %q -> priority %d", a.Email, ov.PlanTier, PromoPriority)
		}
	}

	// 持久化额度快照
	quotaJSON, _ := json.Marshal(ov)
	total, used, remaining := 0.0, 0.0, 0.0
	if ov.Total != nil {
		total = *ov.Total
	}
	if ov.Used != nil {
		used = *ov.Used
	}
	if ov.Remaining != nil {
		remaining = *ov.Remaining
	}
	a.setQuota(string(quotaJSON), ov.PlanTier, ov.PlanExpire, total, used, remaining)
	if err := z.db.SetAccountQuota(a.ID, string(quotaJSON), ov.PlanTier, ov.PlanExpire, total, used, remaining); err != nil {
		log.Printf("[quota] persist %s: %v", a.Email, err)
	}
}

// tryRefreshAccount 用库中 refresh_token 兑换新 JWT（每账号单飞）。
// ok=true：兑换成功——新凭证已落库、账号已恢复 active（内存+DB）。
// inflight=true：另一 goroutine 的刷新已在跑，结果未知——调用方暂不判死。
// 其余失败返回 (false,false)，调用方沿用原 401 处理（MarkInvalid），不会更糟。
// 说明：上游刷新请求形状无仓库内样例，按 token 端点的 JSON 信封风格构造；
// 若形状不符，兑换失败并退回手动重登路径。
func (z *ZCodeAPI) tryRefreshAccount(a *Account) (ok, inflight bool) {
	if a.Provider != "zai" || a.RefreshToken == "" {
		return false, false
	}
	if _, busy := z.tokenRefreshInflight.LoadOrStore(a.ID, struct{}{}); busy {
		return false, true
	}
	defer z.tokenRefreshInflight.Delete(a.ID)

	data, err := z.refreshTokenRequest(a.RefreshToken)
	if err != nil {
		log.Printf("[oauth] refresh %s failed: %v", a.Email, err)
		return false, false
	}
	jwt := jsonStr(data, "token")
	if jwt == "" {
		log.Printf("[oauth] refresh %s: 响应不含新 JWT", a.Email)
		return false, false
	}
	zai, _ := data["zai"].(map[string]interface{})
	newRefresh := firstNonEmpty(jsonStr(zai, "refresh_token"), a.RefreshToken) // 上游可能不轮换
	newAccess := jsonStr(zai, "access_token")
	if err := z.db.UpdateAccountTokens(a.ID, newAccess, newRefresh, jwt, "", ""); err != nil {
		log.Printf("[oauth] refresh %s persist: %v", a.Email, err)
		return false, false
	}
	// 恢复必须同时落内存与 DB：只改内存的话，DB 里的 invalid 状态会让池子在
	// 整个退避周期内继续跳过这个刚修好的账号
	a.setRuntime(StatusActive, "", 0)
	a.setCredentials(newAccess, newRefresh, jwt)
	if err := z.db.SetAccountStatus(a.ID, StatusActive, "", 0); err != nil {
		log.Printf("[oauth] refresh %s status persist: %v", a.Email, err)
	}
	log.Printf("[oauth] refreshed JWT for %s via refresh_token", a.Email)
	return true, false
}

// credentialsAlreadyRotated 本副本的 JWT 落后于库中凭证（刷新成功后才加载的
// 旧快照赶来报 401）时为 true——不得把刚修好的账号按旧凭证判死
func (z *ZCodeAPI) credentialsAlreadyRotated(a *Account) bool {
	if a.ID == 0 || a.ZCodeJWT == "" {
		return false
	}
	fresh, err := z.db.GetAccount(a.ID)
	if err != nil || fresh == nil || fresh.ZCodeJWT == "" {
		return false
	}
	if fresh.ZCodeJWT == a.ZCodeJWT {
		return false
	}
	log.Printf("[quota] account %s credentials already rotated in DB; skipping invalid", a.Email)
	return true
}

// refreshTokenRequest POST /api/v1/oauth/token（refresh_token 兑换）
func (z *ZCodeAPI) refreshTokenRequest(refreshToken string) (map[string]interface{}, error) {
	payload, _ := json.Marshal(map[string]string{
		"provider":      "zai",
		"refresh_token": refreshToken,
	})
	client := ClientForURL(z.egress.GlobalProxyURL(), OAuthTokenURL, 30*time.Second)
	resp, err := client.Post(OAuthTokenURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v map[string]interface{}
	json.Unmarshal(body, &v)
	if len(v) == 0 && resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if codeVal, has := v["code"]; has {
		if n := jsonInt(v, "code"); n != 0 && n != 200 {
			msg := firstNonEmpty(jsonStr(v, "msg"), jsonStr(v, "message"), truncate(string(body), 200))
			return nil, fmt.Errorf("业务码 %d: %s", n, msg)
		}
		_ = codeVal
	}
	data, _ := v["data"].(map[string]interface{})
	if data == nil {
		return nil, fmt.Errorf("响应缺少 data")
	}
	return data, nil
}

// HandleCountTokens POST /v1/messages/count_tokens — Anthropic SDK 会探测该端点。
// 网关拿不到 GLM 分词器，按内容结构估算而非原始 JSON 长度：
// ASCII≈4字符/令牌、CJK≈1字/令牌、其他文字≈2字符/令牌，图片按固定开销计，
// 结果整体上浮 5% 保持偏保守（宁高勿低），避免 SDK 报 405。
func (z *ZCodeAPI) HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Messages json.RawMessage `json:"messages"`
		System   json.RawMessage `json:"system"`
		Tools    json.RawMessage `json:"tools"`
	}
	// body 可选（全空按 0 估算），但必须是合法 JSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	est := estBaseTokens +
		estimateMessagesTokens(body.Messages) +
		estimateContentTokens(body.System) +
		estimateTextTokens(string(body.Tools))
	writeJSON(w, http.StatusOK, map[string]interface{}{"input_tokens": est})
}

// ---- count_tokens 估算辅助 ----

const (
	estImageTokens     = 1500 // 单个图片/文档块的保守令牌开销
	estMessageOverhead = 4    // 每条消息的框架开销（role 等）
	estBaseTokens      = 8    // 请求整体基础开销
)

// estimateTextTokens 按字符类别估算纯文本令牌数
func estimateTextTokens(s string) int {
	ascii, cjk, other := 0, 0, 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r < utf8.RuneSelf:
			ascii++
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r),
			unicode.Is(unicode.Katakana, r), unicode.Is(unicode.Hangul, r):
			cjk++
		default:
			other++
		}
	}
	n := ascii/4 + cjk + other/2
	return (n * 21) / 20 // +5% 保守余量
}

// estimateContentTokens 处理 content/system 字段：字符串或内容块数组
// （text / thinking / tool_use / tool_result / image 等）。结构不识别时按原始长度折半兜底。
func estimateContentTokens(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return estimateTextTokens(s)
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return estimateTextTokens(string(raw)) / 2
	}
	total := 0
	for _, br := range blocks {
		var b map[string]json.RawMessage
		if json.Unmarshal(br, &b) != nil {
			total += estimateTextTokens(string(br)) / 2
			continue
		}
		total += estimateBlockTokens(b, br)
	}
	return total
}

func estimateBlockTokens(b map[string]json.RawMessage, raw json.RawMessage) int {
	var ty string
	_ = json.Unmarshal(b["type"], &ty)
	switch ty {
	case "text":
		return estimateContentTokens(b["text"])
	case "thinking":
		return estimateContentTokens(b["thinking"])
	case "tool_use":
		// input 是任意 JSON 对象：按原始 JSON 文本估算（键名开销小，偏保守方向）
		return estimateTextTokens(string(b["input"]))
	case "tool_result":
		return estimateContentTokens(b["content"])
	case "image", "document":
		return estImageTokens
	default:
		// 未知块类型：整块原始长度折半兜底，不 panic
		return estimateTextTokens(string(raw)) / 2
	}
}

// estimateMessagesTokens 遍历 messages 数组，逐条累加内容与框架开销
func estimateMessagesTokens(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return estimateTextTokens(string(raw)) / 2
	}
	total := 0
	for _, m := range msgs {
		total += estMessageOverhead + estimateContentTokens(m["content"])
	}
	return total
}

// HandleModels GET /v1/models — 同时兼容 OpenAI 与 Anthropic 字段
func (z *ZCodeAPI) HandleModels(w http.ResponseWriter, r *http.Request) {
	models := z.effectiveModels()
	now := time.Now().Unix()
	data := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		data = append(data, modelObject(m, now))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"object": "list", "data": data})
}

// HandleModelRetrieve GET /v1/models/{id} — 部分 SDK 在 list 后按 id 单查模型
func (z *ZCodeAPI) HandleModelRetrieve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	if id == "" || strings.Contains(id, "/") {
		writeAPIError(w, http.StatusNotFound, "not found: "+r.URL.Path)
		return
	}
	for _, m := range z.effectiveModels() {
		if m == id {
			writeJSON(w, http.StatusOK, modelObject(m, time.Now().Unix()))
			return
		}
	}
	writeAPIError(w, http.StatusNotFound, "model not found: "+id)
}

// effectiveModels 生效模型清单：DB gateway_models 设置覆盖优先
func (z *ZCodeAPI) effectiveModels() []string {
	models := z.cfg.GetModels()
	if extra, _ := z.db.GetSetting("gateway_models"); strings.TrimSpace(extra) != "" {
		var list []string
		for _, m := range strings.Split(extra, ",") {
			if m = strings.TrimSpace(m); m != "" {
				list = append(list, m)
			}
		}
		if len(list) > 0 {
			models = list
		}
	}
	return models
}

// modelObject OpenAI / Anthropic 双兼容的单模型对象
func modelObject(id string, now int64) map[string]interface{} {
	return map[string]interface{}{
		"id":           id,
		"object":       "model", // OpenAI 客户端校验字段
		"type":         "model", // Anthropic 客户端校验字段
		"display_name": id,
		"created":      now,
		"created_at":   "2025-01-01T00:00:00Z",
		"owned_by":     "zcode-proxy",
	}
}

// isPromoTier 判断套餐档位是否属于免费促销层（自动降 priority 用）
func isPromoTier(tier string) bool {
	t := strings.ToLower(tier)
	return strings.Contains(t, "start") || strings.Contains(t, "trial") ||
		strings.Contains(t, "promo") || strings.Contains(tier, "体验")
}
