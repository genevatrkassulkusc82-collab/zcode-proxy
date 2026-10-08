package main

// ---- Off-Peak 异步免费通道（B2，移植 zcode-api src/async）----
//
// 上游在闲时开放免费算力队列：先取票（ticket），排队期间客户端连接以
// SSE 注释帧保活，票面 ready 后携带 X-Off-Peak-Ticket-ID 调用
// /api/v1/off-peak/anthropic/v1/messages，结束后 settle 关票。
//
// 控制面（均为 Bearer JWT + x-coding-plan-api-key 鉴权，路径不签名）：
//   GET  /api/v1/off-peak/ticket/availability   探队 {can_take_number,next_take_at}
//   POST /api/v1/off-peak/ticket                取票 {task_id} → {ticket_id,state,...}
//   POST /api/v1/off-peak/ticket/status         批量查票 {ticket_ids:[..]} (≤100)
//   POST /api/v1/off-peak/ticket/{id}/settle    关票（无 body；4xx 视为已清理成功）
//
// 票状态机：queued → ready/active → settled；expired/not_found 需重新取票。
// 服务端 next_poll_after / active_deadline / next_take_at 均为 unix 秒。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	offPeakOrigin        = "https://zcode.z.ai"
	offPeakControlBase   = offPeakOrigin + "/api/v1/off-peak"
	offPeakMessagesURL   = offPeakOrigin + "/api/v1/off-peak/anthropic/v1/messages"
	offPeakControlTo     = 15 * time.Second
	offPeakBatchMax      = 100
	offPeakDefaultPollMS = 1000
	offPeakKeepaliveMS   = 15000
	offPeakMaxRetries    = 3
	offPeakMaxWaitSec    = 1800
)

// 闲时通道专用 outcome（基础集 outcomeWritten/NextAccount/UpstreamError 在 relay.go）。
// 数值与主集错开：二者只做等值比较，不进 switch。
const (
	outcomeTicketRetry     relayOutcome = 101 // 429/3105：票仍有效，同票稍后重试（未写响应）
	outcomeTicketReclaimed relayOutcome = 102 // 400/3102/3001：票被回收，同 task_id 重取（未写响应）
)

// offPeakBodyCode 解析上游 JSON 信封的业务码（非 JSON/缺失返回 0）
func offPeakBodyCode(body []byte) int {
	var v map[string]interface{}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	return jsonInt(v, "code")
}

// ---- 设置 ----

func (z *ZCodeAPI) asyncEnabled() bool {
	v, _ := z.db.GetSetting("async_enabled")
	return v == "1" || v == "true"
}

func (z *ZCodeAPI) asyncIntSetting(key string, def int) int {
	v, err := z.db.GetSetting(key)
	if err != nil || v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return def
}

// ---- 控制面客户端 ----

type offPeakTickets struct{ z *ZCodeAPI }

// do 控制面请求；settle4xxOK 时 4xx 视为服务端已清理的成功（settle 专用）。
// ctx 由调用方提供：取票/查票随请求上下文取消；settle 用 context.Background()
// （常在客户端断开后异步执行，不能被请求取消波及）。
func (t offPeakTickets) do(ctx context.Context, a *Account, method, path string, body interface{}, settle4xxOK bool) (map[string]interface{}, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	urlStr := offPeakControlBase + path
	req, err := http.NewRequestWithContext(ctx, method, urlStr, rdr)
	if err != nil {
		return nil, err
	}
	id := NewClientIdentity(t.z.appVersion, a.DeviceMid)
	for k, v := range ZaiClientHeaders(id) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+a.ZCodeJWT)
	if a.APIKey != "" {
		req.Header.Set("x-coding-plan-api-key", a.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := ClientForURL(t.z.egress.ProxyURLForAccount(a), urlStr, offPeakControlTo)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		if settle4xxOK {
			return map[string]interface{}{}, nil
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var v map[string]interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("响应非 JSON: %s", truncate(string(raw), 120))
	}
	// 信封解包规则（对齐 zcode-api client.ts request）：code===0 且有 data 才解包；
	// code!==0 报错；无 code 的含 data 对象保守不解包
	if _, has := v["code"]; has {
		n := jsonInt(v, "code")
		if n != 0 {
			return nil, fmt.Errorf("业务码 %d: %s", n, firstNonEmpty(jsonStr(v, "msg"), jsonStr(v, "message"), truncate(string(raw), 120)))
		}
		if d, ok := v["data"].(map[string]interface{}); ok {
			return d, nil
		}
	}
	return v, nil
}

// Availability 探队：canTake=false 时返回 nextTakeAt（unix 秒，0=未知）
func (t offPeakTickets) Availability(ctx context.Context, a *Account) (canTake bool, nextTakeAt int64, err error) {
	v, err := t.do(ctx, a, "GET", "/ticket/availability", nil, false)
	if err != nil {
		return false, 0, err
	}
	canTake = v["can_take_number"] == true
	if n, ok := v["next_take_at"].(float64); ok {
		nextTakeAt = int64(n)
	}
	return canTake, nextTakeAt, nil
}

// offTicket 控制面响应的归一化票
type offTicket struct {
	ID             string
	State          string
	Position       int
	NextPollSec    int // 服务端建议的下轮轮询间隔（秒）
	ActiveDeadline int64
}

func offTicketFrom(m map[string]interface{}) (*offTicket, error) {
	id, _ := m["ticket_id"].(string)
	state, _ := m["state"].(string)
	if id == "" || state == "" {
		return nil, fmt.Errorf("票响应缺少 ticket_id/state")
	}
	tk := &offTicket{ID: id, State: state}
	if n, ok := m["position"].(float64); ok {
		tk.Position = int(n)
	}
	if n, ok := m["active_deadline"].(float64); ok {
		tk.ActiveDeadline = int64(n)
	}
	return tk, nil
}

func (t offPeakTickets) take(ctx context.Context, a *Account, taskID string) (*offTicket, error) {
	v, err := t.do(ctx, a, "POST", "/ticket", map[string]interface{}{"task_id": taskID}, false)
	if err != nil {
		return nil, err
	}
	tk, err := offTicketFrom(v)
	if err != nil {
		return nil, err
	}
	if n, ok := v["next_poll_after"].(float64); ok && n > 0 {
		tk.NextPollSec = int(n)
	}
	return tk, nil
}

func (t offPeakTickets) status(ctx context.Context, a *Account, ticketID string) (*offTicket, error) {
	v, err := t.do(ctx, a, "POST", "/ticket/status", map[string]interface{}{"ticket_ids": []string{ticketID}}, false)
	if err != nil {
		return nil, err
	}
	tickets, _ := v["tickets"].([]interface{})
	for _, item := range tickets {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if id, _ := m["ticket_id"].(string); id == ticketID {
			tk, err := offTicketFrom(m)
			if err != nil {
				return nil, err
			}
			if n, ok := v["next_poll_after"].(float64); ok && n > 0 {
				tk.NextPollSec = int(n)
			}
			return tk, nil
		}
	}
	return nil, fmt.Errorf("查票响应不含该票")
}

// offPeakCreds 关票 goroutine 用的凭证快照（无锁读取，避免与 setCredentials 竞争）
type offPeakCreds struct {
	z         *ZCodeAPI
	jwt       string
	apiKey    string
	deviceMid string
	proxyURL  string
}

// settleSnapshot 同 settle，但使用凭证快照（在 spawn 前取好）
func (t offPeakTickets) settleSnapshot(cred offPeakCreds, ticketID string) {
	// 独立 goroutine（net/http 只恢复 handler）：panic 不得带走整个进程
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[async] settle goroutine panic: %v", r)
		}
	}()
	id := NewClientIdentity(cred.z.appVersion, cred.deviceMid)
	settleURL := offPeakControlBase + "/ticket/" + url.QueryEscape(ticketID) + "/settle"
	// 关票是补偿性操作：5xx/网络抖动重试一次，避免仍有效的票占住账号免费取票
	// 配额直到上游过期（4xx = 服务端已清理，不重试）
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(3 * time.Second)
		}
		ctx := context.Background()
		req, err := http.NewRequestWithContext(ctx, "POST", settleURL, nil)
		if err != nil {
			log.Printf("[async] settle %s… failed: %v", safePrefixLog(ticketID, 8), err)
			return
		}
		for k, v := range ZaiClientHeaders(id) {
			req.Header.Set(k, v)
		}
		req.Header.Set("Authorization", "Bearer "+cred.jwt)
		if cred.apiKey != "" {
			req.Header.Set("x-coding-plan-api-key", cred.apiKey)
		}
		client := ClientForURL(cred.proxyURL, settleURL, offPeakControlTo)
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[async] settle %s… failed: %v", safePrefixLog(ticketID, 8), err)
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode < 400 {
			return
		}
		if resp.StatusCode < 500 {
			return // 4xx 视为服务端已清理
		}
		log.Printf("[async] settle %s… got HTTP %d (transient; ticket may linger until upstream expiry)", safePrefixLog(ticketID, 8), resp.StatusCode)
	}
}

func offPeakStateReady(s string) bool    { return s == "ready" || s == "active" }
func offPeakStateExpired(s string) bool  { return s == "expired" || s == "not_found" }
func offPeakStateTerminal(s string) bool { return s == "settled" || offPeakStateExpired(s) }

// offPeakStateKnown 上游已知的全部票状态。未知串（上游新增/改名状态）不得
// 无限占坑轮询——async_max_wait_sec=0 时那是唯一的终止路径
func offPeakStateKnown(s string) bool {
	return s == "queued" || offPeakStateReady(s) || offPeakStateTerminal(s)
}

// ---- 网关入口 ----

// HandleAsyncMessages POST /async/v1/messages — Anthropic 原生协议经闲时队列
func (z *ZCodeAPI) HandleAsyncMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !z.asyncEnabled() {
		writeAPIError(w, http.StatusServiceUnavailable, "async (off-peak) channel is disabled; enable async_enabled in settings")
		return
	}
	body, errResp := readJSONBody(r)
	if errResp != nil {
		errResp.Write(w)
		return
	}
	if provider := detectProvider(body, r.Header); provider != "zai" {
		writeAPIError(w, http.StatusBadRequest, "async channel only supports zai accounts")
		return
	}
	if err := normalizeBody(body, z); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 闲时端点按小写模型名校验（桌面/网关常规通道大小写不敏感，此处敏感）
	if m, ok := body["model"].(string); ok {
		body["model"] = strings.ToLower(m)
	}
	// 命名网关 Key：闲时通道与常规通道同规范拦截（模型白名单 / token 配额），
	// 否则受限 Key 换走 /async 入口即可绕过白名单与配额
	if gk := gatewayKeyFromCtx(r.Context()); gk != nil {
		model, _ := body["model"].(string)
		mrc := &relayCtx{body: body, clientModel: model}
		if errResp := checkGatewayKeyRequest(gk, relayModelName(mrc)); errResp != nil {
			errResp.Write(w)
			return
		}
	}
	// 桌面端 transform 语义：最后一条非 system 消息的最后一个 block 打 ephemeral 缓存标
	if msgs, ok := body["messages"].([]interface{}); ok {
		for i := len(msgs) - 1; i >= 0; i-- {
			mm, ok := msgs[i].(map[string]interface{})
			if !ok || mm["role"] == "system" {
				continue
			}
			if blocks, ok := mm["content"].([]interface{}); ok && len(blocks) > 0 {
				if last, ok := blocks[len(blocks)-1].(map[string]interface{}); ok {
					last["cache_control"] = map[string]interface{}{"type": "ephemeral"}
				}
			} else if s, ok := mm["content"].(string); ok {
				mm["content"] = []interface{}{map[string]interface{}{
					"type": "text", "text": s,
					"cache_control": map[string]interface{}{"type": "ephemeral"},
				}}
			}
			break
		}
	}
	if err := validateMessagesBody(r.Context(), body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 上游闲时端点只接受流式调用（3001 parameter error on stream:false）：
	// 一律强制 stream=true，非流式客户端由网关聚合 SSE 后返回完整 message
	clientStream, _ := body["stream"].(bool)
	body["stream"] = true
	payload, _ := json.Marshal(body)

	z.runOffPeak(w, r, offPeakRunOpts{
		payload:      payload,
		group:        r.Header.Get("x-zcode-group"),
		clientStream: clientStream,
		pollMS:       z.asyncIntSetting("async_poll_interval_ms", offPeakDefaultPollMS),
		keepaliveMS:  z.asyncIntSetting("async_keepalive_ms", offPeakKeepaliveMS),
		maxRetries:   z.asyncIntSetting("async_max_retries", offPeakMaxRetries),
		maxWaitSec:   z.asyncIntSetting("async_max_wait_sec", offPeakMaxWaitSec),
	})
}

type offPeakRunOpts struct {
	payload      []byte
	group        string
	clientStream bool
	pollMS       int
	keepaliveMS  int
	maxRetries   int
	maxWaitSec   int
	hardDeadline time.Time // 请求级排队总预算（runOffPeak 设定一次）
}

// runOffPeak 选号并执行闲时桥接。闲时是独立免费额度桶（billing 耗尽仍可取票，
// 见 /api/offpeak/availability），因此选号只要求启用 + JWT，不做 exhausted/cooling 门禁。
func (z *ZCodeAPI) runOffPeak(w http.ResponseWriter, r *http.Request, opts offPeakRunOpts) {
	tickets := offPeakTickets{z: z}
	// x-zcode-group 与 /v1 同语义：逗号分隔多组的账号按组名逐段匹配
	// （ListAccounts 的 SQL 精确等值会漏掉 "eu,failover" 这类多组账号）
	allAccounts, err := z.db.ListAccounts("")
	if err != nil {
		log.Printf("[async] list accounts: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"type": "error", "error": map[string]string{"message": "账号查询失败", "type": "no_available_account"},
		})
		return
	}
	accounts := allAccounts
	if opts.group != "" {
		accounts = nil
		for _, a := range allAccounts {
			if matchAccountGroup(a, opts.group) {
				accounts = append(accounts, a)
			}
		}
	}
	tried := map[int64]bool{}
	attempts := 0
	var reasons []string
	streamHeaders := false // SSE 响应头只写一次：换号续流时不重复 WriteHeader
	// 排队总时长预算整个请求一次（不随换号重置），否则最坏等待 = 每账号各等满额
	if opts.maxWaitSec > 0 {
		opts.hardDeadline = time.Now().Add(time.Duration(opts.maxWaitSec) * time.Second)
	}
	// 轮转起点：固定 DB 顺序会让并发闲时请求全部压在第一个账号上
	if n := len(accounts); n > 1 {
		start := int(z.asyncRotation.Add(1)) % n
		accounts = append(accounts[start:], accounts[:start]...)
	}
	for _, a := range accounts {
		if attempts >= maxAccountAttempts {
			break
		}
		if !a.Enabled || a.Provider != "zai" || a.ZCodeJWT == "" || tried[a.ID] {
			continue
		}
		tried[a.ID] = true
		attempts++
		// 换号间隙不沉默：取票/兑换最长 30-45s，先补一帧保活防中间层掐断
		if opts.clientStream && streamHeaders {
			fmt.Fprint(w, ": switching account\n\n")
			flushWriter(w)
		}
		out := z.offPeakBridge(w, r, a, tickets, opts, &streamHeaders)
		if out == outcomeWritten {
			return
		}
		// 客户端已断开（bridge 通过 outcomeUpstreamError 报告）：停止消耗账号与免费票
		if out == outcomeUpstreamError || r.Context().Err() != nil {
			return
		}
		st, lastErr := a.statusError()
		reasons = append(reasons, a.DisplayNameOrEmail()+": "+firstNonEmpty(lastErr, st))
	}
	detail := truncate(strings.Join(dedup(reasons), "；"), 400)
	// 如实区分"全部失败"与"达到单次尝试上限还有账号没试"
	msg := "闲时通道暂不可用（所有账号取票失败），请稍后重试或改用 /v1/messages"
	if attempts >= maxAccountAttempts && len(reasons) >= maxAccountAttempts {
		msg = "闲时通道已尝试 " + strconv.Itoa(attempts) + " 个账号达到单次请求上限，其余账号本次未尝试，请稍后重试或改用 /v1/messages"
	} else if attempts == 0 {
		msg = "闲时通道暂无可用账号（全部禁用或无 JWT），请在后台检查账号状态"
	}
	if detail != "" {
		msg += "（最近失败原因: " + detail + "）"
	}
	log.Printf("[async] no available account: %s", detail)
	if opts.clientStream {
		if !streamHeaders {
			// 没有任何 bridge 执行过（如账号全被过滤）：先补 SSE 响应头再写错误帧，
			// 否则客户端收到的是隐式 200 + text/plain，SDK 无法按 SSE 解析
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			flushWriter(w)
		}
		writeSSEErrorEvent(w, msg)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		"type": "error", "error": map[string]string{"message": msg, "type": "no_available_account"},
	})
}

// offPeakBridge 单账号闲时桥接状态机：
// WAIT(排队，保活) → READY(带票调用上游) → EXPIRED/回收(重新取票，≤maxRetries)
// → DONE(流结束，settle) / ABORT(客户端断开，settle)。
// 返回 outcomeWritten 表示响应已按协议写回客户端（成功或终态错误）；
// outcomeNextAccount 表示本账号不可用（未写响应），由 runOffPeak 换号重试；
// outcomeUpstreamError 表示客户端已断开。
func (z *ZCodeAPI) offPeakBridge(w http.ResponseWriter, r *http.Request, a *Account,
	tickets offPeakTickets, opts offPeakRunOpts, streamHeaders *bool) relayOutcome {

	ctx := r.Context()
	pollInterval := time.Duration(opts.pollMS) * time.Millisecond
	if pollInterval < 500*time.Millisecond {
		pollInterval = 500 * time.Millisecond
	}
	keepalive := time.Duration(opts.keepaliveMS) * time.Millisecond
	if keepalive < 3*time.Second {
		keepalive = 3 * time.Second
	}
	maxRetries := opts.maxRetries
	if maxRetries < 0 {
		maxRetries = offPeakMaxRetries
	}
	hardDeadline := opts.hardDeadline // 请求级预算，由 runOffPeak 统一设定

	if opts.clientStream && !*streamHeaders {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flushWriter(w)
		*streamHeaders = true
	}

	// settle 在独立 goroutine 读凭证：快照后再 spawn，避免与
	// tryRefreshAccount 的 setCredentials 并发读写同一 Account
	jwt, apiKey, deviceMid := a.credentialSnapshot()
	cred := offPeakCreds{z: z, jwt: jwt, apiKey: apiKey, deviceMid: deviceMid,
		proxyURL: z.egress.ProxyURLForAccount(a)} // 关票与取票同出口，维持 IP 一致性
	settled := map[string]bool{}
	settleOnce := func(ticketID string) {
		if ticketID == "" || settled[ticketID] {
			return
		}
		settled[ticketID] = true
		// 关票凭证取"当下"快照：401 恢复路径的 setCredentials 会就地更新本
		// 副本，桥起点快照的旧 JWT 若已被上游吊销，关票 401 会被 4xx 分支
		// 吞成"服务端已清理"——票实际挂着占坑到 active_deadline，零日志。
		// credentialSnapshot 锁保护，可安全跨 goroutine 调；proxyURL 维持取票同出口
		jwt2, apiKey2, deviceMid2 := a.credentialSnapshot()
		if jwt2 == "" {
			jwt2 = jwt
		}
		go tickets.settleSnapshot(offPeakCreds{z: z, jwt: jwt2, apiKey: apiKey2,
			deviceMid: deviceMid2, proxyURL: cred.proxyURL}, ticketID)
	}
	// settleCur 关闭"当前"票（闭包捕获变量，随 retake 更新）。
	// 只在终态路径调用：失败 / 放弃 / 客户端断开 / 响应完成——绝不取票即关。
	var (
		ticket *offTicket
		err    error
	)
	settleCur := func() {
		if ticket != nil {
			settleOnce(ticket.ID)
		}
	}
	fail := func(status int, msg string) relayOutcome {
		settleCur()
		a.bumpFail(truncate(msg, 180))
		// bumpFail 只改内存副本；同步推进 DB 的 fail_count/last_error，
		// 否则仪表盘看不到反复超时的账号
		if err := z.db.BumpAccountFail(a.ID, truncate(msg, 180)); err != nil {
			log.Printf("[async] bump fail persist %s: %v", a.DisplayNameOrEmail(), err)
		}
		log.Printf("[async] account %s bridge failed: %s", a.DisplayNameOrEmail(), msg)
		if opts.clientStream {
			writeSSEErrorEvent(w, msg)
		} else {
			writeJSON(w, status, map[string]interface{}{
				"type": "error", "error": map[string]string{"message": msg, "type": "async_error"},
			})
		}
		return outcomeWritten
	}
	// takeFailed 取票失败：先判客户端取消（不得因此冷却健康账号），
	// 401/403 先试 refresh_token 兑换（与主转发路径同一恢复优先策略），
	// 其余按错误类型标记（429 短冷却、其余失败/冷却），
	// 未写任何响应，交给 runOffPeak 换下一个账号——不得终止整个请求
	takeFailed := func(err error) relayOutcome {
		if ctx.Err() != nil {
			// 客户端已断开：账号无辜，runOffPeak 收到该 outcome 会停止循环
			return outcomeUpstreamError
		}
		msg := "闲时取票失败: " + truncate(err.Error(), 160)
		s := err.Error()
		switch {
		case strings.Contains(s, "HTTP 401"), strings.Contains(s, "HTTP 403"):
			if ok, inflight := z.tryRefreshAccount(a); ok || inflight {
				return outcomeNextAccount
			}
			if z.credentialsAlreadyRotated(a) {
				return outcomeNextAccount
			}
			z.pool.MarkInvalid(a, msg)
		case strings.Contains(s, "HTTP 429"):
			// 取票 429 = 队列饱和（与转发路径 3105 同性质），不是账号健康问题：
			// 全局冷却会把 /async 的拥塞传染给主 /v1 免费通道（账号物理健康）
			log.Printf("[async] account %s take rate-limited (queue saturation), no cooldown: %s", a.DisplayNameOrEmail(), truncate(msg, 120))
		case strings.Contains(s, "HTTP"):
			z.pool.MarkFailed(a, msg)
		default:
			z.pool.MarkCooling(a, msg, 60)
		}
		log.Printf("[async] account %s take failed, trying next: %s", a.DisplayNameOrEmail(), msg)
		return outcomeNextAccount
	}
	// 官方语义：同一任务的 retake 复用同一 task_id（offPeakTaskService：
	// "票据过期…同 task_id 重取号（已确认允许多次）"）；每次换新 id 会额外
	// 占用服务端每账号任务配额（取号超限以 3103 暴露）
	taskID := uuid.NewString()
	// retake 重新取票：返回 (nil, outcome) 表示本账号到此为止——
	// outcome 为 outcomeNextAccount 时未写响应，由 runOffPeak 换号；
	// outcome 为 outcomeWritten 时 fail() 已写终态错误（排队超时）。
	retake := func(attempt int, why string) (*offTicket, relayOutcome) {
		if attempt >= maxRetries {
			// 本账号重试预算用尽：关掉手上可能仍活着的票（如按本地时钟误判
			// 过 deadline 的 ready 票），标记后换号，不写终态响应
			settleCur()
			z.pool.MarkFailed(a, truncate(why+"且重试次数用尽", 180))
			return nil, outcomeNextAccount
		}
		if !hardDeadline.IsZero() && time.Now().After(hardDeadline) {
			return nil, fail(http.StatusServiceUnavailable, why+"；闲时排队总时长已超限")
		}
		settleCur() // 旧票已被回收/过期，显式关掉
		next, err := tickets.take(ctx, a, taskID)
		if err != nil {
			return nil, takeFailed(fmt.Errorf("%s；重新取票失败: %w", why, err))
		}
		log.Printf("[async] account %s retake #%d: %s… (%s)", a.DisplayNameOrEmail(), attempt+1, safePrefixLog(next.ID, 8), next.State)
		return next, outcomeNextAccount // 成功持有新票；调用方只看 next != nil
	}

	// ticket/err 由下方取票赋值；必须用 = 以让 settleCur 闭包捕获同一变量
	ticket, err = tickets.take(ctx, a, taskID)
	if err != nil {
		return takeFailed(err)
	}
	log.Printf("[async] account %s took ticket %s… state=%s pos=%d", a.DisplayNameOrEmail(), safePrefixLog(ticket.ID, 8), ticket.State, ticket.Position)

	pollFailures := 0
	nextKeepalive := time.Time{} // 跨轮询迭代持有：保活到期不因每次调用重派而失效
	lastReclaimCode := 0         // 最近一次票回收的业务码（offPeakForward 落笔，诊断串用）
	for attempt := 0; ; attempt++ {
		unknownStates := 0 // 每张新票独立计数（≥2 视同 expired，防无限轮询）
		// WAIT：票未就绪时轮询 + 保活
		for !offPeakStateReady(ticket.State) && !offPeakStateTerminal(ticket.State) {
			if ctx.Err() != nil {
				settleCur() // 对端已断开，放弃当前票
				return outcomeUpstreamError
			}
			if !hardDeadline.IsZero() && time.Now().After(hardDeadline) {
				return fail(http.StatusServiceUnavailable, "闲时排队超时，请稍后重试或改用 /v1/messages")
			}
			sleep := pollInterval
			if ticket.NextPollSec > 0 {
				sleep = time.Duration(ticket.NextPollSec) * time.Second
			}
			// 服务端建议的轮询间隔是变量（官方客户端钳制 5s~5min）：
			// 不设上界时一条 next_poll_after=3600 能把 30min 预算顶穿 2 倍，
			// max_wait_sec=0 时更完全不设限；且 sleep 须让位于硬截止
			if sleep > 5*time.Minute {
				sleep = 5 * time.Minute
			}
			if !hardDeadline.IsZero() {
				if d := time.Until(hardDeadline); d < sleep {
					sleep = d
				}
			}
			if !offPeakWait(ctx, sleep, keepalive, func() { fmt.Fprint(w, ": keepalive\n\n"); flushWriter(w) }, opts.clientStream, &nextKeepalive) {
				settleCur()
				return outcomeUpstreamError
			}
			st, err := tickets.status(ctx, a, ticket.ID)
			if err != nil {
				// 查票连续失败设上限：maxWaitSec=0（无时间预算）时防无限占坑轮询
				pollFailures++
				log.Printf("[async] poll ticket %s…: %v", safePrefixLog(ticket.ID, 8), err)
				if pollFailures >= 5 {
					return fail(http.StatusBadGateway, "闲时查票连续失败: "+truncate(err.Error(), 160))
				}
				continue
			}
			pollFailures = 0
			ticket = st
			// 未知状态连续两次即视同 expired（单次可能是瞬时怪响应，
			// 不立即丢弃有效排队票；两次则几乎必然是上游状态机变更）
			if offPeakStateKnown(ticket.State) {
				unknownStates = 0
			} else if unknownStates++; unknownStates >= 2 {
				log.Printf("[async] ticket %s… unknown state %q twice, treating as expired", safePrefixLog(ticket.ID, 8), ticket.State)
				break // 交由下方 expired 同路分支 retake
			}
		}

		if offPeakStateExpired(ticket.State) || ticket.State == "settled" || !offPeakStateKnown(ticket.State) {
			next, out := retake(attempt, fmt.Sprintf("闲时票被回收（%s）", ticket.State))
			if next == nil {
				return out
			}
			ticket = next
			pollFailures = 0 // 新票重新计数
			continue
		}
		if ticket.ActiveDeadline > 0 && time.Now().Unix() > ticket.ActiveDeadline {
			next, out := retake(attempt, "闲时票已过使用截止时间")
			if next == nil {
				return out
			}
			ticket = next
			pollFailures = 0
			continue
		}
		out := z.offPeakForward(w, r, a, ticket, opts, &lastReclaimCode)
		if out == outcomeTicketRetry {
			// 429/3105：票仍有效，同票稍后重试（不关票）。
			// 预算在此强制执行——attempt 随循环增长但 retake 不经过此路径，
			// 不检查则 async_max_wait_sec=0 时同一张票可无限重试
			if attempt >= maxRetries {
				settleCur()
				z.pool.MarkFailed(a, "闲时通道同票重试预算用尽（429/3105）")
				return outcomeNextAccount
			}
			continue
		}
		if out == outcomeTicketReclaimed {
			// 400/3102/3001：票被上游回收但任务有效——同 task_id 重取续跑。
			// lastReclaimCode 由 offPeakForward 落笔（顺序循环，无竞争），
			// 诊断串带真实业务码而不是硬编码 3102
			next, rout := retake(attempt, fmt.Sprintf("票被上游回收（%d）", lastReclaimCode))
			if next == nil {
				settleCur()
				return rout
			}
			ticket = next
			pollFailures = 0
			continue
		}
		// 票已带入 READY/ACTIVE：所有出口都关票（幂等 best-effort）。
		// 换号/断开路径不关票会泄漏票面，占用该账号的免费队列直到 active_deadline。
		settleCur()
		return out
	}
}

// offPeakWait 等待 min(sleep) 或保活周期；emitKeepalive 在保活到期时被调用。
// nextKeepalive 由调用方跨调用持有（零值 = 首次，按 keepalive 初始化）——
// 在函数内重派会让秒级轮询下保活永不触发，中间设备空闲超时掐断连接
// 返回 false 表示客户端已断开。
func offPeakWait(ctx context.Context, sleep, keepalive time.Duration, emitKeepalive func(), stream bool, nextKeepalive *time.Time) bool {
	deadline := time.Now().Add(sleep)
	if nextKeepalive != nil && nextKeepalive.IsZero() {
		*nextKeepalive = time.Now().Add(keepalive)
	}
	if !stream {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-timer.C:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		now := time.Now()
		if !now.Before(deadline) {
			return true
		}
		wait := deadline.Sub(now)
		if keepaliveDue := nextKeepalive.Sub(now); keepaliveDue < wait {
			wait = keepaliveDue
		}
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			if !time.Now().Before(*nextKeepalive) && time.Now().Before(deadline) {
				emitKeepalive()
				*nextKeepalive = time.Now().Add(keepalive)
			}
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return false
		}
	}
}

// offPeakForward READY → 带票调用上游闲时消息端点并透传响应（协议两侧同为 Anthropic）。
// reclaimCode 非 nil 时回写票回收的业务码（3102/3001），供调用方诊断串使用
func (z *ZCodeAPI) offPeakForward(w http.ResponseWriter, r *http.Request, a *Account, ticket *offTicket, opts offPeakRunOpts, reclaimCode *int) relayOutcome {
	start := time.Now()

	// metadata.user_id：桌面端所有 Anthropic 调用都携带（JSON 字符串形式的
	// {device_id, account_uuid, session_id}），off-peak 端点对其做参数校验
	bodyBytes := opts.payload
	var m map[string]interface{}
	if json.Unmarshal(bodyBytes, &m) == nil && m != nil {
		if _, has := m["metadata"]; !has {
			m["metadata"] = map[string]interface{}{
				"user_id": fmt.Sprintf(`{"device_id":%q,"account_uuid":"","session_id":""}`, a.DeviceMid),
			}
			if raw, merr := json.Marshal(m); merr == nil {
				bodyBytes = raw
			}
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), "POST", offPeakMessagesURL, bytes.NewReader(bodyBytes))
	if err != nil {
		if opts.clientStream {
			writeSSEErrorEvent(w, "上游请求构造失败")
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"type": "error", "error": map[string]string{"message": "上游请求构造失败", "type": "async_error"},
			})
		}
		return outcomeWritten
	}
	id := NewClientIdentity(z.appVersion, a.DeviceMid)
	for k, v := range ZaiClientHeaders(id) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+a.ZCodeJWT)
	if a.APIKey != "" {
		req.Header.Set("x-coding-plan-api-key", a.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersionH)
	req.Header.Set("X-ZCode-Agent", "glm")
	req.Header.Set("X-Off-Peak-Ticket-ID", ticket.ID)
	req.Header.Set("Accept", "text/event-stream")

	client := ClientForURL(z.egress.ProxyURLForAccount(a), offPeakMessagesURL, 0)
	resp, err := client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return outcomeUpstreamError
		}
		z.pool.MarkCooling(a, "闲时通道连接失败: "+truncate(err.Error(), 160), 60)
		return outcomeNextAccount
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		// Cloudflare/WAF 挑战页与凭证失效同状态码：先按挑战分类（与主转发
		// relay 同款），不能掉进下方 401/403 分支把健康账号误标 invalid
		chalBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if isCloudflareChallenge(resp.Header, string(chalBody)) {
			z.pool.MarkCooling(a, fmt.Sprintf("Cloudflare/WAF 挑战 HTTP %d（闲时通道）", resp.StatusCode), 300)
			return outcomeNextAccount
		}
		// 与主转发/取票路径同一恢复优先策略：先试 refresh_token 兑换
		if ok, inflight := z.tryRefreshAccount(a); ok || inflight {
			return outcomeNextAccount
		}
		if z.credentialsAlreadyRotated(a) {
			return outcomeNextAccount
		}
		z.pool.MarkInvalid(a, fmt.Sprintf("闲时通道鉴权失败 HTTP %d", resp.StatusCode))
		return outcomeNextAccount
	case resp.StatusCode == 429:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if offPeakBodyCode(body) == 3105 {
			// 官方契约（offPeakMockGateway）：429/3105 + Retry-After = 票仍有效，
			// 仅上游饱和 → 同票稍后重试，而不是废弃仍有效的票、冷却健康账号
			wait := 15 * time.Second
			if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
				if sec, perr := strconv.Atoi(ra); perr == nil && sec > 0 && sec <= 45 {
					wait = time.Duration(sec) * time.Second
				}
			}
			log.Printf("[async] 429/3105 upstream saturation; same-ticket retry in %s", wait)
			nextKA := time.Time{}
			if !offPeakWait(r.Context(), wait, 15*time.Second, func() {
				if opts.clientStream {
					fmt.Fprint(w, ": keepalive\n\n")
					flushWriter(w)
				}
			}, opts.clientStream, &nextKA) {
				return outcomeUpstreamError
			}
			return outcomeTicketRetry
		}
		z.pool.MarkCooling(a, "闲时通道限流 429", 30)
		return outcomeNextAccount
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if code := offPeakBodyCode(body); code == 3102 || code == 3001 {
			// 官方契约：400/3102 = 票已被回收但任务有效 → 同 task_id 重新取票续跑
			// （3001 为滚动发布期旧语义，官方适配器同样按票过期处理），
			// 而不是把整段排队等待作废成硬 400
			if reclaimCode != nil {
				*reclaimCode = code
			}
			log.Printf("[async] %d: ticket reclaimed upstream; retaking with same task_id", code)
			return outcomeTicketReclaimed
		}
		z.pool.MarkFailed(a, fmt.Sprintf("闲时通道上游错误 HTTP %d", resp.StatusCode))
		z.recordUsage(a, r, opts.payload, resp.StatusCode, start, 0, nil, opts.clientStream)
		msg := "闲时通道上游错误 HTTP " + strconv.Itoa(resp.StatusCode) + ": " + truncate(string(body), 300)
		if opts.clientStream {
			writeSSEErrorEvent(w, msg)
		} else {
			writeJSON(w, resp.StatusCode, map[string]interface{}{
				"type": "error", "error": map[string]string{"message": msg, "type": "api_error"},
			})
		}
		return outcomeWritten
	}

	z.pool.MarkUsed(a)

	// 2xx 但非 SSE（WAF 挑战页/登录页 HTML，主转发路径同样把守）：不得把
	// 风控页面聚合成"成功空响应"或原样灌进客户端 SSE 流
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		z.pool.MarkCooling(a, fmt.Sprintf("闲时通道返回非 SSE 内容（%s）", truncate(ct, 60)), 120)
		msg := "闲时通道上游返回异常内容 HTTP " + strconv.Itoa(resp.StatusCode) + ": " + truncate(string(body), 300)
		if opts.clientStream {
			writeSSEErrorEvent(w, msg)
			return outcomeWritten
		}
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"type": "error", "error": map[string]string{"message": msg, "type": "api_error"},
		})
		return outcomeWritten
	}

	if opts.clientStream {
		sniff := newUsageSniffReader(resp.Body)
		flushWriter(w)
		buf := make([]byte, 32<<10)
		truncated := false
		ttft := 0 // 首字节（转发侧）：主路径有计，闲时路径此前恒 0 污染看板
		for {
			n, rerr := sniff.Read(buf)
			if n > 0 {
				if ttft == 0 {
					ttft = int(time.Since(start).Milliseconds())
				}
				if _, werr := w.Write(buf[:n]); werr != nil {
					z.recordUsage(a, r, opts.payload, resp.StatusCode, start, ttft, sniff.usage(), opts.clientStream)
					return outcomeWritten
				}
				flushWriter(w)
			}
			if rerr != nil {
				truncated = !errors.Is(rerr, io.EOF)
				break
			}
		}
		recStatus := resp.StatusCode
		// 中途断流或上游内联错误帧（嗅探到 ERR: 前缀）：补协议正确的 error 帧
		// 并把记录状态修正为 502——不得把半截流伪装成干净的 200 成功
		sniffed := sniff.usage()
		inlineErr := sniffed != nil && strings.HasPrefix(sniffed.StopReason, "ERR:")
		if truncated || inlineErr {
			msg := "上游流中断"
			if inlineErr {
				msg = strings.TrimPrefix(sniffed.StopReason, "ERR:")
			} else {
				msg = "upstream stream interrupted"
			}
			writeSSEErrorEvent(w, msg)
			recStatus = http.StatusBadGateway
		}
		z.recordUsage(a, r, opts.payload, recStatus, start, ttft, sniffed, opts.clientStream)
		return outcomeWritten
	}
	// 非流式客户端：上游强制流式返回，网关聚合成完整 Anthropic message。
	// 64MB 上限与 relay 非流式路径同规：多读 1 字节判定超限——不设界时
	// 超长生成 × 并发请求会无界吃内存
	aggregated, usage, aerr := aggregateAnthropicStream(io.LimitReader(resp.Body, (64<<20)+1))
	if aerr == nil && len(aggregated) > 64<<20 {
		aerr = fmt.Errorf("上游响应超过 64MB 上限")
	}
	if aerr != nil {
		// 聚合失败也要落已提取的部分用量，不得整条丢失
		z.recordUsage(a, r, opts.payload, http.StatusBadGateway, start, 0, usage, false)
		msg := "闲时通道响应聚合失败: " + truncate(aerr.Error(), 200)
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"type": "error", "error": map[string]string{"message": msg, "type": "api_error"},
		})
		return outcomeWritten
	}
	z.recordUsage(a, r, opts.payload, resp.StatusCode, start, 0, usage, false)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	w.Write(aggregated)
	return outcomeWritten
}

// ---- SSE 聚合（非流式客户端）----

// anthropicAgg 将上游 Anthropic SSE 流聚合为一条完整 message JSON
type anthropicAgg struct {
	id, model                string
	blocks                   []map[string]interface{}
	stopReason               string
	stopSequence             interface{} // message_delta.stop_sequence 回显（客户端需要知道哪条命中）
	sawMessageDelta          bool        // message_delta 到达过（干净的完整流必有；缺失 = 中途截断）
	streamError              string      // 上游内联错误帧（独立于 stop_reason，不被后续 delta 掩盖）
	toolTruncated            bool        // 不可解析的非空工具参数缓冲（评审 F7：截断，不得洗成空参成功）
	in, out                  int
	cacheRead, cacheCreation int
	toolJSON                 map[int]*strings.Builder
}

func aggregateAnthropicStream(r io.Reader) ([]byte, *StreamUsage, error) {
	p := &sseParser{}
	agg := &anthropicAgg{toolJSON: map[int]*strings.Builder{}}
	onEvent := func(ev sseEvent) {
		switch ev.Event {
		case "message_start":
			if msg, ok := ev.Data["message"].(map[string]interface{}); ok {
				agg.id, _ = msg["id"].(string)
				agg.model, _ = msg["model"].(string)
				if u, ok := msg["usage"].(map[string]interface{}); ok {
					if n := toInt(u["input_tokens"]); n > agg.in {
						agg.in = n
					}
					if n := toInt(u["cache_read_input_tokens"]); n > agg.cacheRead {
						agg.cacheRead = n
					}
					if n := toInt(u["cache_creation_input_tokens"]); n > agg.cacheCreation {
						agg.cacheCreation = n
					}
				}
			}
		case "content_block_start":
			idx := toInt(ev.Data["index"])
			blk, _ := ev.Data["content_block"].(map[string]interface{})
			if blk == nil {
				return
			}
			nb := map[string]interface{}{"type": blk["type"]}
			switch blk["type"] {
			case "text":
				nb["text"] = ""
			case "thinking":
				nb["thinking"] = ""
				// R6：start 自带的签名必须保留，聚合后的 message 才能在下一轮重放
				if sig, ok := blk["signature"].(string); ok && sig != "" {
					nb["signature"] = sig
				}
			case "tool_use":
				nb["id"] = blk["id"]
				nb["name"] = blk["name"]
				// start 帧可能自带完整 input（上游偶发行为，convert.go 同守卫）：
				// 先落盘，无 delta 或 delta 解析失败时兜底
				if input, ok := blk["input"].(map[string]interface{}); ok {
					nb["input"] = input
				} else {
					nb["input"] = map[string]interface{}{}
				}
				agg.toolJSON[idx] = &strings.Builder{}
			case "redacted_thinking":
				// 透传 data：剥掉会产出缺字段的块，下一轮重放必被上游拒绝
				if data, ok := blk["data"]; ok {
					nb["data"] = data
				}
			}
			for idx >= len(agg.blocks) {
				agg.blocks = append(agg.blocks, nil)
			}
			agg.blocks[idx] = nb
		case "content_block_delta":
			idx := toInt(ev.Data["index"])
			delta, _ := ev.Data["delta"].(map[string]interface{})
			// idx < 0 或块类型与 delta 不匹配（上游违例帧）不得 panic
			if delta == nil || idx < 0 || idx >= len(agg.blocks) || agg.blocks[idx] == nil {
				return
			}
			switch delta["type"] {
			case "signature_delta":
				// R6：签名增量同样聚合（上游可能不走 start 携带）
				if s, ok := delta["signature"].(string); ok && s != "" {
					prev, _ := agg.blocks[idx]["signature"].(string)
					agg.blocks[idx]["signature"] = prev + s
				}
			case "text_delta":
				if s, ok := delta["text"].(string); ok {
					if prev, ok2 := agg.blocks[idx]["text"].(string); ok2 {
						agg.blocks[idx]["text"] = prev + s
					}
				}
			case "thinking_delta":
				if s, ok := delta["thinking"].(string); ok {
					if prev, ok2 := agg.blocks[idx]["thinking"].(string); ok2 {
						agg.blocks[idx]["thinking"] = prev + s
					}
				}
			case "input_json_delta":
				if b, ok := agg.toolJSON[idx]; ok {
					if s, ok := delta["partial_json"].(string); ok {
						b.WriteString(s)
					}
				}
			}
		case "content_block_stop":
			idx := toInt(ev.Data["index"])
			if b, ok := agg.toolJSON[idx]; ok {
				// 仅在有增量缓冲时覆盖：上游常在 start 帧自带完整 input（已落盘
				// 兜底），空缓冲解不出东西，覆盖会把合法 input 冲成 {}
				if raw := b.String(); raw != "" {
					var parsed interface{}
					if err := json.Unmarshal([]byte(raw), &parsed); err != nil || parsed == nil {
						// 字面量 "null"（Unmarshal 成功但得 nil）：空对象容忍；
						// 解析失败 = 参数中途断流（评审 F7）：置位，聚合末尾按失败处理
						if parsed == nil {
							parsed = map[string]interface{}{}
						} else {
							agg.toolTruncated = true
						}
					}
					agg.blocks[idx]["input"] = parsed
				}
				delete(agg.toolJSON, idx)
			}
		case "message_delta":
			if u, ok := ev.Data["usage"].(map[string]interface{}); ok {
				if v, ok := u["output_tokens"]; ok {
					agg.out = toInt(v)
				}
				if v, ok := u["input_tokens"]; ok {
					if n := toInt(v); n > agg.in {
						agg.in = n
					}
				}
				if v, ok := u["cache_read_input_tokens"]; ok {
					if n := toInt(v); n > agg.cacheRead {
						agg.cacheRead = n
					}
				}
				if v, ok := u["cache_creation_input_tokens"]; ok {
					if n := toInt(v); n > agg.cacheCreation {
						agg.cacheCreation = n
					}
				}
			}
			if d, ok := ev.Data["delta"].(map[string]interface{}); ok {
				agg.sawMessageDelta = true
				if sr, ok := d["stop_reason"].(string); ok && sr != "" {
					agg.stopReason = sr
				}
				if ss, has := d["stop_sequence"]; has {
					agg.stopSequence = ss
				}
			}
		case "error":
			msg := "upstream stream error"
			if e, ok := ev.Data["error"].(map[string]interface{}); ok {
				if m, ok := e["message"].(string); ok && m != "" {
					msg = m
				}
			}
			agg.streamError = msg
		}
	}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			// 解析缓冲溢出（>16MB 无帧边界）必须按失败处理：
			// 静默吞掉会把半截数据聚合成"成功空响应"
			if ferr := p.feed(buf[:n], onEvent); ferr != nil {
				return nil, partialUsage(agg), ferr
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return nil, partialUsage(agg), rerr
			}
			break
		}
	}
	p.flush(onEvent)
	// 干净 EOF 但缺 content_block_stop（上游违例）：冲刷残留的工具参数缓冲，
	// 否则完整 JSON 被丢弃、合成出 input:{} 的 tool_use（客户端空参执行工具）
	if len(agg.toolJSON) > 0 {
		idxs := make([]int, 0, len(agg.toolJSON))
		for idx := range agg.toolJSON {
			idxs = append(idxs, idx)
		}
		sort.Ints(idxs)
		for _, idx := range idxs {
			if b, ok := agg.toolJSON[idx]; ok {
				if idx >= len(agg.blocks) || agg.blocks[idx] == nil {
					delete(agg.toolJSON, idx)
					continue
				}
				// 同 content_block_stop：空缓冲不覆盖 start 帧自带的合法 input；
				// 解析失败 = 截断置位（评审 F7）
				if raw := b.String(); raw != "" {
					var parsed interface{}
					if err := json.Unmarshal([]byte(raw), &parsed); err != nil || parsed == nil {
						if parsed == nil {
							parsed = map[string]interface{}{}
						} else {
							agg.toolTruncated = true
						}
					}
					agg.blocks[idx]["input"] = parsed
				}
				delete(agg.toolJSON, idx)
			}
		}
	}
	content := make([]interface{}, 0, len(agg.blocks))
	for _, b := range agg.blocks {
		if b != nil {
			content = append(content, b)
		}
	}
	if agg.streamError != "" {
		return nil, partialUsage(agg), fmt.Errorf("%s", agg.streamError)
	}
	// 工具参数截断（评审 F7）：聚合出 input:{} 的 tool_use 会让客户端空参执行
	// 工具——与中途断流同规按失败处理，不产出"成功"message
	if agg.toolTruncated {
		return nil, partialUsage(agg), fmt.Errorf("upstream tool arguments truncated")
	}
	// 零事件的干净 EOF（裸注释帧后直接关闭）：聚合成 200 空 message 同样是
	// "把死流伪装成成功"——不满足 message_start 必至的一律按失败处理
	if agg.id == "" && len(agg.blocks) == 0 {
		return nil, partialUsage(agg), fmt.Errorf("upstream stream ended without any events")
	}
	// 有内容但没等到 message_delta（干净截断）：合成 end_turn 会把半截回答
	// 伪装成完整消息、把截断的 tool_use 变成空参执行——与透传路径的
	// truncated 判定同规按失败处理
	if !agg.sawMessageDelta && agg.stopReason == "" {
		return nil, partialUsage(agg), fmt.Errorf("upstream stream truncated before message_delta")
	}
	sr := agg.stopReason
	if sr == "" {
		sr = "end_turn"
	}
	out := map[string]interface{}{
		"id": agg.id, "type": "message", "role": "assistant", "model": agg.model,
		"content":       content,
		"stop_reason":   sr,
		"stop_sequence": agg.stopSequence,
		"usage": map[string]interface{}{
			"input_tokens": agg.in, "output_tokens": agg.out,
			"cache_read_input_tokens": agg.cacheRead, "cache_creation_input_tokens": agg.cacheCreation,
		},
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, partialUsage(agg), err
	}
	return raw, &StreamUsage{InputTokens: agg.in, OutputTokens: agg.out,
		CacheReadTokens: agg.cacheRead, CacheCreationTokens: agg.cacheCreation}, nil
}

// partialUsage 聚合失败时返回已提取的部分用量（可为 nil），供失败路径落库
func partialUsage(agg *anthropicAgg) *StreamUsage {
	if agg.in == 0 && agg.out == 0 {
		return nil
	}
	return &StreamUsage{InputTokens: agg.in, OutputTokens: agg.out,
		CacheReadTokens: agg.cacheRead, CacheCreationTokens: agg.cacheCreation}
}

// ---- SSE 透传期间的 usage 嗅探 ----

// usageSniffReader 透传字节流的同时嗅探 Anthropic SSE usage 帧（message_start/message_delta）
type usageSniffReader struct {
	r      io.Reader
	acc    StreamUsage
	peeked string
	done   bool
}

func newUsageSniffReader(r io.Reader) *usageSniffReader { return &usageSniffReader{r: r} }

func (u *usageSniffReader) usage() *StreamUsage {
	u.flushPeek()
	// 无 usage 帧但有内联错误：照样返回——否则 ERR: 标记随 nil 丢失，
	// 失败流会被记成干净的 200 成功
	if u.acc.InputTokens == 0 && u.acc.OutputTokens == 0 && !strings.HasPrefix(u.acc.StopReason, "ERR:") {
		return nil
	}
	cp := u.acc
	return &cp
}

func (u *usageSniffReader) flushPeek() {
	for {
		idx := strings.IndexByte(u.peeked, '\n')
		if idx < 0 {
			return
		}
		line := strings.TrimRight(u.peeked[:idx], "\r")
		u.peeked = u.peeked[idx+1:]
		u.sniffLine(line)
	}
}

func (u *usageSniffReader) sniffLine(line string) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(line[5:])
	if data == "" || data == "[DONE]" {
		return
	}
	var v struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message struct {
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if json.Unmarshal([]byte(data), &v) != nil {
		return
	}
	switch v.Type {
	case "error":
		// 上游内联错误帧：记入 StopReason，让用量记录如实反映失败
		if m := v.Error.Message; m != "" {
			u.acc.StopReason = "ERR:" + m
		} else if u.acc.StopReason == "" || !strings.HasPrefix(u.acc.StopReason, "ERR:") {
			u.acc.StopReason = "ERR:upstream stream error"
		}
	default:
		// 无 type 但带 error 键的帧（网关封套形状不一，与 isErrorEnvelope
		// 同理）：同样记 ERR 标记，失败流不得被记成干净的 200 成功
		if v.Type == "" && v.Error.Message != "" {
			u.acc.StopReason = "ERR:" + v.Error.Message
		}
	case "message_start":
		if v.Message.Usage.InputTokens > u.acc.InputTokens {
			u.acc.InputTokens = v.Message.Usage.InputTokens
		}
		// R3 缓存计量：与主路径同 max 语义（否则闲时流的缓存 token 恒为 0）
		if v.Message.Usage.CacheReadInputTokens > u.acc.CacheReadTokens {
			u.acc.CacheReadTokens = v.Message.Usage.CacheReadInputTokens
		}
		if v.Message.Usage.CacheCreationInputTokens > u.acc.CacheCreationTokens {
			u.acc.CacheCreationTokens = v.Message.Usage.CacheCreationInputTokens
		}
	case "message_delta":
		if v.Usage.OutputTokens > 0 {
			u.acc.OutputTokens = v.Usage.OutputTokens
		}
		if v.Usage.InputTokens > u.acc.InputTokens {
			u.acc.InputTokens = v.Usage.InputTokens
		}
		if v.Usage.CacheReadInputTokens > u.acc.CacheReadTokens {
			u.acc.CacheReadTokens = v.Usage.CacheReadInputTokens
		}
		if v.Usage.CacheCreationInputTokens > u.acc.CacheCreationTokens {
			u.acc.CacheCreationTokens = v.Usage.CacheCreationInputTokens
		}
		// 已标记的内联错误（ERR: 前缀）不被后续 delta 的 stop_reason 掩盖
		if v.Delta.StopReason != "" && !strings.HasPrefix(u.acc.StopReason, "ERR:") {
			u.acc.StopReason = v.Delta.StopReason
		}
	}
}

func (u *usageSniffReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	if n > 0 && !u.done {
		u.peeked += string(p[:n])
		if len(u.peeked) > 1<<20 {
			// 嗅探缓冲超限：放弃解析，避免内存膨胀
			u.done = true
			u.peeked = ""
		} else {
			u.flushPeek()
		}
	}
	return n, err
}

// writeSSEErrorEvent 以 Anthropic 风格 SSE error 帧收尾（不得伪装成功）
func writeSSEErrorEvent(w http.ResponseWriter, msg string) {
	enc, _ := json.Marshal(msg)
	fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%s}}\n\n", enc)
	flushWriter(w)
}

func flushWriter(w http.ResponseWriter) {
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}
