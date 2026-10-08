package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ---- 2API 转发核心 ----
// 移植 zcode2api gateway.py：多账号轮询 + 额度用完自动换号 + 验证码降级链。
// 降级链：JWT+验证码 → JWT 不带验证参数直连 → API Key 回退端点（api.z.ai）。

const (
	maxCaptchaRetries  = 3
	maxAccountAttempts = 5
	// 32MB：GLM-5.3 百万 token 上下文 + ~227 个工具 schema + base64 图片的
	// 合法大请求可能越过 8MB；413 对客户端是不可重试硬失败（轮 7）。
	// 出站上限仍为 64MB；本地个人代理，JSON 解析的内存尖峰可接受
	maxRequestBytes = 32 << 20
)

// modelNameMap 上游模型名大小写敏感，客户端小写别名 → 官方名
var modelNameMap = map[string]string{
	"glm-5.3":       "GLM-5.3",
	"glm-5.2":       "GLM-5.2",
	"glm-5-turbo":   "GLM-5-Turbo",
	"glm-turbo":     "GLM-5-Turbo",
	"glm-5.1":       "GLM-5.1",
	"glm-4.7":       "GLM-4.7",
	"glm-4.6":       "GLM-4.6",
	"glm-4.5":       "GLM-4.5",
	"glm-4.5-air":   "GLM-4.5-Air",
	"glm-4.5v":      "GLM-4.5V",
	"glm-4.5-flash": "GLM-4.5-Flash",
}

// relayOutcome 单次转发结果
type relayOutcome int

const (
	outcomeWritten         relayOutcome = iota // 响应已写回客户端
	outcomeNextAccount                         // 账号不可用，换下一个
	outcomeCaptchaRejected                     // 验证码被拒，尝试下一条路径
	outcomeUpstreamError                       // 上游最终错误（已写回）
	outcomeRiskBlocked                         // 风控拦截（3012），尝试本账号下一条路径
)

// 转发模式（通道选择，见 pool.go 的 paid_fallback_mode 策略）
const (
	relayModeFree     = "free"     // 仅免费通道（JWT 两条路径）
	relayModePaid     = "paid"     // 仅付费通道（API Key 回退端点）
	relayModeBalanced = "balanced" // 软受限（风控/验证码）就地级联付费；硬失败交阶段二
)

// protocol 客户端协议类型（决定响应转换）
type protocol int

const (
	protocolAnthropic protocol = iota
	protocolOpenAI
	protocolResponses
	protocolCompletions
)

// relayCtx 一次转发请求的上下文
type relayCtx struct {
	body         map[string]interface{} // 已规范化的 Anthropic 格式请求体
	provider     string
	group        string
	proto        protocol
	clientStream bool   // 客户端是否要 SSE
	clientModel  string // 回显给客户端的模型名
	includeUsage bool   // OpenAI stream_options.include_usage
	echo         bool   // /v1/completions echo=true：choices.text 前缀原 prompt
	prompt       string // /v1/completions 原始 prompt（echo 回显用）
	// sawRateLimit 本次请求内任一账号/通道撞过上游限流（HTTP 429 或业务码）。
	// 全部账号耗尽时用于区分"过载"（→ 529 overloaded_error，客户端有专门的
	// 过载重试分类与文案）与"无可用账号"（→ 503）
	sawRateLimit bool
	// sawNonRateCooldown 本次请求内出现过非限流类的账号冷却（鉴权失效/额度
	// 耗尽/风控/连接失败等）。与 sawRateLimit 同时为真 = 混合故障：终态按 503
	// 如实回报，不得谎报成纯过载（轮 4 红队 F4）
	sawNonRateCooldown bool
}

// HandleMessages POST /v1/messages — 原生 Anthropic 协议
func (z *ZCodeAPI) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, errResp := readJSONBody(r)
	if errResp != nil {
		errResp.Write(w)
		return
	}
	provider := detectProvider(body, r.Header)
	if err := normalizeBody(body, z); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 网关 Key 白名单在请求校验前拦截（轮 8）：注定 403 的请求不得先触发
	// URL 图片抓取等准备工作。RPM/配额仍在 relay 内计数，此处仅白名单
	if gk := gatewayKeyFromCtx(r.Context()); gk != nil {
		model, _ := body["model"].(string)
		if name := canonicalModelName(model); !gatewayKeyModelAllowed(gk, name) {
			writeAPIError(w, http.StatusForbidden, "model not allowed for this gateway key: "+name)
			return
		}
	}
	if err := validateMessagesBody(r.Context(), body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	clientStream, _ := body["stream"].(bool)
	model, _ := body["model"].(string)
	rc := &relayCtx{
		body: body, provider: provider, group: r.Header.Get("x-zcode-group"),
		proto: protocolAnthropic, clientStream: clientStream, clientModel: model,
	}
	z.relay(w, r, rc)
}

// relay 选账号并按降级链转发。
// 通道模型（free_first 默认策略）：免费通道（JWT 套餐额度）先用；免费侧受限
// （并发满/限流/额度耗尽/风控）后无缝落到付费通道（api.z.ai API Key，按量计费）。
// 并发闸门按通道隔离，因此部分会话走免费、部分走付费可同时成立。
func (z *ZCodeAPI) relay(w http.ResponseWriter, r *http.Request, rc *relayCtx) {
	// 命名网关 Key（R1）：模型白名单 / token 配额在进入账号池前拦截
	if gk := gatewayKeyFromCtx(r.Context()); gk != nil {
		if errResp := checkGatewayKeyRequest(gk, relayModelName(rc)); errResp != nil {
			errResp.Write(w)
			return
		}
	}
	payload, _ := json.Marshal(rc.body)
	var reasons []string
	start := time.Now()
	sessionKey := rc.sessionKey()
	policy := z.pool.PaidFallbackPolicy()
	// 本请求的全量账号快照：一次取用，选择/503 提示全程复用（热路径上
	// 逐次 ListAccounts 的逐行 vault 解密是最坏情形的主要 CPU 开销）。
	// 取舍：本请求内其他请求对账号的标记不可见（skip 只挡本请求已试过的），
	// 最坏多烧几次受限账号的往返，下次请求自愈；后台任务一律用库内新副本
	accounts := z.pool.SnapshotAccounts()

	// 阶段一：免费通道。balanced 策略下免费侧"软受限"（风控拦截/验证码拒绝）
	// 立刻同账号试付费通道；硬失败（耗尽/限流/凭证失效）与 free_first 一致交给阶段二
	freeMode := relayModeFree
	if policy == PaidModeBalanced {
		freeMode = relayModeBalanced
	}
	tried := map[int64]bool{}
	for attempt := 0; attempt < maxAccountAttempts; attempt++ {
		a := z.pool.SelectStickyChannel(rc.provider, rc.group, sessionKey, accounts, tried, ChannelFree)
		if a == nil {
			break
		}
		tried[a.ID] = true
		outcome := z.tryAccount(w, r, a, payload, rc, &reasons, start, freeMode)
		if outcome == outcomeWritten || outcome == outcomeUpstreamError {
			return
		}
	}

	// paidDailyTokenCap 是计费通道（channel=paid）唯一的消费闸：对 never 模式的
	// keyonly 循环同样生效（纯 API Key 账号的流量全走 paid 归因，不设闸即失控）。
	// 查询失败按已达上限处理（fail closed）——静默放行会在存储故障期间持续产生计费流量
	paidCapReason := z.paidDailyCapReason()

	// never 策略不进付费阶段，但纯 API Key 账号（无 JWT，付费是唯一通道）仍须可服务
	if policy == PaidModeNever && paidCapReason == "" {
		for attempt := 0; attempt < maxAccountAttempts; attempt++ {
			a := z.pool.SelectStickyChannel(rc.provider, rc.group, sessionKey, accounts, tried, ChannelPaidOnly)
			if a == nil {
				break
			}
			tried[a.ID] = true
			outcome := z.tryAccount(w, r, a, payload, rc, &reasons, start, relayModePaid)
			if outcome == outcomeWritten || outcome == outcomeUpstreamError {
				return
			}
		}
	}

	// 阶段二：付费回退。走到这里说明免费通道没能写回任何响应（全部受限）。
	// 策略开关与每日 token 上限都在进入阶段前拦截。
	triedPaid := map[int64]bool{}
	paidSkipReason := paidCapReason
	if paidSkipReason == "" {
		switch {
		case policy == PaidModeNever:
			paidSkipReason = "付费回退已关闭（仅免费模式）"
		}
	}
	if paidSkipReason == "" {
		if policy == PaidModeBalanced {
			// balanced 阶段一只对软受限（风控/验证码）就地试过付费通道；硬失败的
			// 账号付费通道其实未打过，但保持历史级联语义：本请求不再回头重打
			for id := range tried {
				triedPaid[id] = true
			}
		}
		for attempt := 0; attempt < maxAccountAttempts; attempt++ {
			a := z.pool.SelectStickyChannel(rc.provider, rc.group, sessionKey, accounts, triedPaid, ChannelPaid)
			if a == nil {
				break
			}
			triedPaid[a.ID] = true
			if len(triedPaid) == 1 {
				log.Printf("[relay] free channel limited (%d account(s) tried), falling back to paid channel", len(tried))
			}
			outcome := z.tryAccount(w, r, a, payload, rc, &reasons, start, relayModePaid)
			if outcome == outcomeWritten || outcome == outcomeUpstreamError {
				return
			}
		}
	}

	// rune 安全截断：失败原因以中文为主，按字节切会切碎 UTF-8 尾巴
	detail := truncate(strings.Join(dedup(reasons), "；"), 400)
	msg := "所有账号均不可用或额度已用完，请在后台检查账号状态"
	// 达到单次尝试上限时如实说明：仅尝试了部分账号，其余本次未尝试
	totalTried := len(tried) + len(triedPaid)
	if totalTried >= maxAccountAttempts {
		msg = fmt.Sprintf("已尝试 %d 个账号/通道达到单次请求上限，其余本次未尝试，请稍后重试或在后台检查账号状态", totalTried)
	}
	if paidSkipReason != "" {
		msg += "；" + paidSkipReason
	}
	// 若因冷却导致无可用账号，给出预计恢复时间
	coolSecs := int64(0)
	if until, reason := z.pool.CoolingInfo(rc.provider, rc.group, accounts); until > 0 {
		coolSecs = until - time.Now().Unix()
		if coolSecs < 0 {
			coolSecs = 0
		}
		msg += fmt.Sprintf("；免费通道冷却中（%s），约 %d 秒后自动恢复重试", firstNonEmpty(reason, "上游限流/风控"), coolSecs)
	}
	// F3：耗尽账号的上游重置时间已知时如实告知（monitor 通道 nextResetTime）。
	// 不带账号邮箱/展示名：503 体面向命名 Key 持有方（可能发给第三方）
	if until, _ := z.pool.ExhaustedResetInfo(rc.provider, rc.group, accounts); until > 0 {
		mins := (until - time.Now().Unix()) / 60
		if mins < 0 {
			mins = 0
		}
		msg += fmt.Sprintf("；免费额度窗口约 %d 分钟后重置", mins)
	}
	if detail != "" {
		msg += "（最近失败原因: " + detail + "）"
	}
	log.Printf("[relay] no available account: %s", detail)
	writeAllAccountsUnavailable(w, rc, msg, paidSkipReason != "", coolSecs)
}

// writeAllAccountsUnavailable 终态"无账号可用"响应。Anthropic 协议且本次请求见过
// 上游限流时按 529 overloaded_error 回报：zai-org/ZCode 客户端对 529 有专门的
// 过载分类（可重试、文案 "Provider is overloaded"），并对 429/529 解析 Retry-After
// 精确退避；一律 503 会让它退回通用指数退避。付费闸拦截（paidSkipped）说明根因
// 含策略/配额而非纯过载，维持 503。错误体用 Anthropic 原生 {"type":"error",...} 形状。
func writeAllAccountsUnavailable(w http.ResponseWriter, rc *relayCtx, msg string, paidSkipped bool, coolSecs int64) {
	// 同为 Anthropic 信封：客户端 schema 要求顶层 type:"error"，否则
	// no_available_account 的明细（冷却/重置时间）到不了用户眼前。
	// 混合故障（限流+鉴权失效/耗尽等）按 503 如实回报，不谎报纯过载
	if rc.proto == protocolAnthropic && rc.sawRateLimit && !rc.sawNonRateCooldown && !paidSkipped {
		retryAfter := 10
		if coolSecs >= 1 && coolSecs <= 300 {
			retryAfter = int(coolSecs)
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		log.Printf("[relay] all accounts rate-limited → 529 overloaded_error (retry-after=%ds)", retryAfter)
		writeJSON(w, 529, map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": "overloaded_error", "message": msg},
		})
		return
	}
	// 同为 Anthropic 信封：客户端 schema 要求顶层 type:"error"，否则
	// no_available_account 的明细（冷却/重置时间）到不了用户眼前
	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		"type":  "error",
		"error": map[string]string{"message": msg, "type": "no_available_account"},
	})
}

// canonicalModelName 白名单校验用的规范模型名：去 provider 前缀 + 小写
//（与 gateway_keys 白名单同规范）
func canonicalModelName(bodyModel string) string {
	if i := strings.Index(bodyModel, "/"); i >= 0 {
		bodyModel = bodyModel[i+1:]
	}
	if official, ok := modelNameMap[strings.ToLower(strings.TrimSpace(bodyModel))]; ok {
		return strings.ToLower(official)
	}
	return strings.ToLower(strings.TrimSpace(bodyModel))
}

func relayModelName(rc *relayCtx) string {
	model := rc.clientModel
	if m, ok := rc.body["model"].(string); ok && m != "" {
		model = m // normalizeBody 已归一化，优先取
	}
	return canonicalModelName(model)
}

// sessionKey 会话粘滞键（F2）：优先 metadata.user_id（Anthropic 客户端语义），
// 缺省退化为 system 块摘要哈希——同一系统提示词的会话视为同一粘滞域。
func (rc *relayCtx) sessionKey() string {
	if md, ok := rc.body["metadata"].(map[string]interface{}); ok {
		if uid := jsonStr(md, "user_id"); uid != "" {
			return "u:" + uid
		}
	}
	if sys, has := rc.body["system"]; has {
		raw, err := json.Marshal(sys)
		if err == nil && len(raw) > 0 {
			sum := sha256.Sum256(raw)
			return "s:" + hex.EncodeToString(sum[:])[:24]
		}
	}
	return ""
}

// tryAccount 单账号转发，按 mode 决定动用哪些通道：
//
//	free     仅免费通道（JWT+验证码 → JWT 直连）；失败统一交给付费回退阶段
//	paid     仅付费通道（API Key 回退端点）；免费侧受限的账号正是要兜底的对象
//	balanced 免费侧软受限（风控/验证码）立刻试同账号付费通道；硬失败（耗尽/限流/401）
//	         不就地级联，统一交给付费回退阶段
//
// 实测免费通道要求人机校验（验证码参数 45s 内可复用），直连仅作放宽时的快速路径。
// 风控拦截（3012）不立即冷却：先试完本模式内其余路径，全部失败才冷却。
func (z *ZCodeAPI) tryAccount(w http.ResponseWriter, r *http.Request, a *Account,
	payload []byte, rc *relayCtx, reasons *[]string, start time.Time, mode string) relayOutcome {

	note := func(msg string) {
		// reasons 会拼进客户端可见的 503 明细，不含账号身份（命名 Key 可能发给
		// 第三方）：用内部 ID 代号，真实展示名只在服务端日志
		*reasons = append(*reasons, fmt.Sprintf("账号#%d: %s", a.ID, msg))
	}

	// 付费阶段：仅 API Key 通道
	if mode == relayModePaid {
		return z.tryPaidChannel(w, r, a, payload, rc, start, note)
	}

	riskBlocked := false

	needsCaptcha := rc.provider == "zai" && a.AuthType == "jwt" && a.ZCodeJWT != ""

	// 路径 1：JWT + 阿里云无痕验证码（含失效重解重试）；无可用验证参数时跳过（与路径 2 直连等价）。
	// 求解等待上限 45s（文档承诺值）：ctx 超时即放弃，在跑尝试转后台入缓存
	if needsCaptcha {
		captchaCtx, captchaCancel := context.WithTimeout(r.Context(), captchaSolveBudgetFront)
		verifyParam, region, err := z.captcha.GetVerifyParamCtx(captchaCtx, a)
		captchaCancel()
		if err != nil {
			note("人机校验求解失败: " + truncate(err.Error(), 180))
		} else if verifyParam != "" {
			out := z.forwardOnce(w, r, a, payload, verifyParam, region, false, maxCaptchaRetries, rc, start, "jwt-captcha", ChannelFree)
			switch out {
			case outcomeWritten, outcomeUpstreamError:
				return out
			case outcomeNextAccount:
				st, lastErr := a.statusError()
				note("账号不可用: " + firstNonEmpty(lastErr, st))
				return outcomeNextAccount
			case outcomeRiskBlocked:
				riskBlocked = true
				note("免费通道风控拦截（unusual activity）")
			case outcomeCaptchaRejected:
				note("带验证码请求被上游拒绝")
			}
		}
	}

	// 路径 2：JWT 不带验证参数直连（上游放宽时零延迟）
	if a.ZCodeJWT != "" && !riskBlocked {
		out := z.forwardOnce(w, r, a, payload, "", "", false, 2, rc, start, "jwt-direct", ChannelFree)
		switch out {
		case outcomeWritten, outcomeUpstreamError:
			return out
		case outcomeNextAccount:
			st, lastErr := a.statusError()
			note("账号不可用: " + firstNonEmpty(lastErr, st))
			return outcomeNextAccount
		case outcomeRiskBlocked:
			riskBlocked = true
			note("免费通道风控拦截（unusual activity）")
		case outcomeCaptchaRejected:
			note("直连被要求人机校验")
		}
	}

	// free 模式：免费通道到此为止。付费通道交给 relay 的付费回退阶段统一调度；
	// 免费通道被风控标记则按阶梯冷却（R4），避免下个请求重复吃风控
	if mode == relayModeFree {
		if riskBlocked {
			z.pool.MarkRiskCooling(a, "免费通道风控拦截（unusual activity）")
			rc.sawNonRateCooldown = true
		}
		log.Printf("[relay] account %s free channel failed (paid fallback deferred)", a.Email)
		return outcomeNextAccount
	}

	// 路径 3（balanced 模式）：API Key 回退端点（api.z.ai，无需验证码，独立于免费通道风控）
	if a.APIKey != "" {
		out := z.forwardOnce(w, r, a, payload, "", "", true, 2, rc, start, "apikey", ChannelPaid)
		switch out {
		case outcomeWritten, outcomeUpstreamError:
			return out
		case outcomeNextAccount:
			st, lastErr := a.statusError()
			note("API Key 回退失败: " + firstNonEmpty(lastErr, st))
			return outcomeNextAccount
		case outcomeRiskBlocked:
			z.pool.MarkPaidRiskCooling(a, "付费通道风控拦截（unusual activity）")
			rc.sawNonRateCooldown = true
			note("API Key 通道也被风控拦截")
		case outcomeCaptchaRejected:
			note("API Key 回退被拒（captcha required）")
		}
	} else if !needsCaptcha {
		note("无 API Key 可回退")
	}

	// 所有路径失败：若是风控拦截则按阶梯冷却（R4：120s → 30min → 24h）
	if riskBlocked {
		z.pool.MarkRiskCooling(a, "上游风控拦截（unusual activity），全通道失败")
		rc.sawNonRateCooldown = true
	}
	log.Printf("[relay] account %s all paths failed", a.Email)
	return outcomeNextAccount
}

// paidDailyCapReason is shared by phase selection and every paid dispatch.
func (z *ZCodeAPI) paidDailyCapReason() string {
	cap := z.pool.paidDailyTokenCap()
	if cap < 0 {
		return "付费通道当日 token 上限状态不可知（fail closed）"
	}
	if cap > 0 {
		used, err := z.db.PaidTokensToday()
		if err != nil {
			log.Printf("[relay] paid tokens today: %v; treating daily cap as exceeded", err)
			return "付费通道当日 token 上限状态不可知（fail closed）"
		}
		if used >= cap {
			return fmt.Sprintf("付费通道已达当日 token 上限（%s）", truncate(strconv.FormatInt(cap, 10), 20))
		}
	}
	return ""
}

// tryPaidChannel 付费阶段单账号：仅 API Key 通道。
// 免费侧状态（exhausted/cooling/invalid）的账号也会被选进来——那正是付费回退要兜底的场景。
func (z *ZCodeAPI) tryPaidChannel(w http.ResponseWriter, r *http.Request, a *Account,
	payload []byte, rc *relayCtx, start time.Time, note func(string)) relayOutcome {

	if a.APIKey == "" {
		note("无 API Key，付费通道不可用")
		return outcomeNextAccount
	}
	out := z.forwardOnce(w, r, a, payload, "", "", true, 2, rc, start, "apikey", ChannelPaid)
	switch out {
	case outcomeWritten, outcomeUpstreamError:
		return out
	case outcomeNextAccount:
		_, lastErr := a.statusError()
		note("付费通道不可用: " + firstNonEmpty(lastErr, "未知"))
		return outcomeNextAccount
	case outcomeRiskBlocked:
		z.pool.MarkPaidRiskCooling(a, "付费通道风控拦截（unusual activity）")
		rc.sawNonRateCooldown = true
		note("付费通道风控拦截（unusual activity）")
		return outcomeNextAccount
	}
	note("付费通道被上游拒绝（captcha required）")
	return outcomeNextAccount
}

// forwardOnce 单条路径转发（含验证码失效重解重试）；pathLabel 用于日志，
// channel 决定并发闸门、受限标记与用量归因落在免费还是付费通道
func (z *ZCodeAPI) forwardOnce(w http.ResponseWriter, r *http.Request, a *Account,
	payload []byte, verifyParam, region string, useFallback bool, retries int,
	rc *relayCtx, start time.Time, pathLabel, channel string) relayOutcome {

	// 每（账号×通道）并发闸门：排队而非打满并发（上游 1302 并发超限的根治手段）。
	// 排队 45s 仍无名额或客户端已断开 → 让位下一候选（10s 短冷却，很快回来）。
	// release 绑定同一闸门：上限变更仍保留全部在途占用
	release, ok := z.pool.AcquireAccountSlot(r.Context(), a, channel, 45*time.Second)
	if !ok {
		// 客户端已断开（ctx 取消/超时）：与下方 client.Do 的取消处理同规——
		// 不动账号状态直接终止。否则一次断连会给排队中的健康账号泼一轮假
		// “并发已满”10s 冷却（逐个污染候选账号，最多 ~10 个/断连）
		if r.Context().Err() != nil {
			return outcomeUpstreamError
		}
		if channel == ChannelPaid {
			z.pool.MarkPaidCooling(a, "付费通道并发已满（在途请求达到上限），短暂冷却", 10)
		} else {
			z.pool.MarkCooling(a, "并发已满（在途请求达到上限），短暂冷却", 10)
		}
		return outcomeNextAccount
	}
	defer release()

	// 内部对 OpenAI/Responses 协议一律流式请求上游，便于聚合与转换
	upstreamStream := rc.clientStream || rc.proto != protocolAnthropic

	for attempt := 0; attempt < retries; attempt++ {
		if channel == ChannelPaid {
			// Reload after queueing and before every retry; use one current snapshot.
			current, err := z.db.GetAccount(a.ID)
			if err != nil {
				log.Printf("[relay] paid account reload failed: %v", err)
				return outcomeNextAccount
			}
			// Only sole-channel accounts bypass the fallback toggle, not cooldowns.
			now := time.Now().Unix()
			if !channelSelectable(current, ChannelPaidOnly, now) && !paidChannelAvailable(current, now) {
				return outcomeNextAccount
			}
			if reason := z.paidDailyCapReason(); reason != "" {
				log.Printf("[relay] paid dispatch blocked: %s", reason)
				return outcomeNextAccount
			}
			a = current
		}
		// 用量按实际通道归因（付费通道按量计费，paid_daily_token_cap 依赖此标记）
		a.setUsageChannel(channel)
		urlStr, headers := z.buildUpstreamRequest(a, verifyParam, region, useFallback, r, upstreamStream)
		req, err := http.NewRequestWithContext(r.Context(), "POST", urlStr, bytes.NewReader(payload))
		if err != nil {
			// 本地构造错误（配置问题），不归咎账号
			log.Printf("[relay] build request failed: %v", err)
			writeAPIError(w, http.StatusInternalServerError, "上游请求构造失败")
			return outcomeUpstreamError
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		// 流式无总超时，靠 context；非流式走慢速 TTFB transport：GLM 思考型
		// 非流式调用（客户端验证/compaction 回退）首字节可达分钟级，复用流式
		// 的 60s ResponseHeaderTimeout 会在思考中途杀请求 → 客户端整单重试烧账号
		proxyURL := z.egress.ProxyURLForAccount(a)
		var client *http.Client
		if upstreamStream {
			client = ClientForURL(proxyURL, urlStr, 0)
		} else {
			client = ClientForURLSlowTTFB(proxyURL, urlStr)
		}
		resp, err := client.Do(req)
		if err != nil {
			// 客户端断连/取消：不动账号状态，直接终止（不写响应，对端已走）
			if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
				return outcomeUpstreamError
			}
		// 完整错误（含出口代理地址）只进服务端日志；客户端可见明细经
		// connFailReason 脱敏——失败原因会拼进 503/529 body 发给命名 Key 持有方
		log.Printf("[relay] account %s connect failed via %s: %v", a.DisplayNameOrEmail(), redactProxyURL(proxyURL), err)
			z.markChannelFailure(rc, a, channel, connFailReason(proxyURL, err), 60)
			return outcomeNextAccount
		}

		// 3xx：WAF 挑战/登录页重定向（客户端已禁重定向），视为上游异常
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			resp.Body.Close()
			z.markChannelFailure(rc, a, channel, fmt.Sprintf("上游重定向 HTTP %d（疑似 WAF 挑战）", resp.StatusCode), 120)
			return outcomeNextAccount
		}

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			text := string(body)

			// Cloudflare/WAF 人机挑战页：出口 IP/风控档位问题，与凭证无关——
			// 既不能走下方 401/403 分支误杀账号，也不能当验证码被拒触发重解；
			// 只冷却本账号该通道（同出口其他账号/路径先顶上）
			if isCloudflareChallenge(resp.Header, text) {
				z.markChannelFailure(rc, a, channel, fmt.Sprintf("Cloudflare/WAF 挑战 HTTP %d", resp.StatusCode), 300)
				return outcomeNextAccount
			}

			// 无 CF 指纹但返回 HTML：阿里云 WAF（上游跑在阿里云，拦截页不带 CF
			// 指纹）、其他防护或中间代理错误页。API 端点正常只回 JSON，4xx/5xx
			// + HTML 与凭证无关，与上方 CF 分支同待遇：只冷通道，绝不判死账号，
			// 也不当验证码被拒
			if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
				z.markChannelFailure(rc, a, channel, fmt.Sprintf("上游返回 HTML 拦截/错误页 HTTP %d", resp.StatusCode), 300)
				return outcomeNextAccount
			}

			// 验证码被拒：失效缓存 → 重解 → 带新参数重试本路径
			if isCaptchaError(text) && (resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403) {
				if verifyParam != "" && attempt+1 < retries {
					z.captcha.InvalidateFor(a)
					log.Printf("[relay] account %s captcha rejected, re-solving", a.Email)
					captchaCtx, captchaCancel := context.WithTimeout(r.Context(), captchaSolveBudgetFront)
					newParam, newRegion, err := z.captcha.GetVerifyParamCtx(captchaCtx, a)
					captchaCancel()
					if err == nil && newParam != "" {
						verifyParam, region = newParam, newRegion
						continue
					}
				}
				return outcomeCaptchaRejected
			}

			// 风控拦截（3012）先于凭证分支判类：上游曾以 401/403 壳携带 3012 体，
			// 落进凭证分支会烧一次 90s 刷新退避再把临时风控标记的账号标成
			// invalid（需人工干预）；短冷却后真死的凭证会再次失败，无害
			if isRiskBlocked(text) {
				log.Printf("[relay] account %s risk-blocked on %s path", a.DisplayNameOrEmail(), pathLabel)
				return outcomeRiskBlocked
			}

			switch {
			case resp.StatusCode == 401 || resp.StatusCode == 403:
				if channel == ChannelPaid {
					// API Key 自身鉴权失败：与免费侧凭证互不相干，只冷付费通道
					//（1h：坏 Key 不会自愈，但留窗口给用户换 Key）
					z.pool.MarkPaidCooling(a, fmt.Sprintf("付费通道鉴权失败 HTTP %d（API Key 无效或被禁）", resp.StatusCode), 3600)
					rc.sawNonRateCooldown = true
					return outcomeNextAccount
				}
				// 先尝试 refresh_token 兑换；成功或已有并发刷新在跑则不判死，
				// 交回池子换号续用（下一轮用新凭证）
				if ok, inflight := z.tryRefreshAccount(a); ok || inflight {
					return outcomeNextAccount
				}
				if z.credentialsAlreadyRotated(a) {
					return outcomeNextAccount
				}
				z.pool.MarkInvalid(a, fmt.Sprintf("鉴权失败 HTTP %d", resp.StatusCode))
				rc.sawNonRateCooldown = true
				return outcomeNextAccount
			case resp.StatusCode == 429 || isRateLimitBody(resp.StatusCode, text):
				// 限流（HTTP 429 或业务码 1302/1303 并发超限）：请求内退避重试一次
				//（尊重 Retry-After），仍失败则按历史冷却时长升级 30s → 120s → 300s。
				// 记录过载信号：全账号耗尽时终态按 529 overloaded_error 回报
				rc.sawRateLimit = true
				retryAfter := 2
				if ra := resp.Header.Get("Retry-After"); ra != "" {
					if n, err := strconv.Atoi(ra); err == nil && n > 0 && n <= 5 {
						retryAfter = n
					}
				}
				if attempt+1 < retries {
					log.Printf("[relay] account %s model %s rate-limited (HTTP %d), retrying in %ds", a.DisplayNameOrEmail(), rcModel(payload), resp.StatusCode, retryAfter)
					resp.Body.Close()
					// 客户端已断开就不必再占重试窗口（与账号槽位队列同理）
					select {
					case <-time.After(time.Duration(retryAfter) * time.Second):
					case <-r.Context().Done():
						return outcomeUpstreamError
					}
					continue
				}
				resp.Body.Close() // 最后一次重试也必须关 body，否则泄漏连接
				reason := fmt.Sprintf("上游限流（HTTP %d，model=%s）", resp.StatusCode, rcModel(payload))
				if channel == ChannelPaid {
					z.pool.MarkPaidCooling(a, reason+", 冷却", z.pool.nextPaidCooldown(a))
				} else {
					cool := nextRateLimitCooldown(a)
					z.pool.MarkCooling(a, fmt.Sprintf("%s，冷却 %ds", reason, cool), cool)
				}
				return outcomeNextAccount
			case isExhaustedError(resp.StatusCode, text):
				if channel == ChannelPaid {
					// 付费通道余额/额度不足：长冷却留充值自愈窗口，免费侧不受牵连
					z.pool.MarkPaidExhausted(a, "付费通道余额/额度不足")
					rc.sawNonRateCooldown = true
					return outcomeNextAccount
				}
				z.pool.MarkExhausted(a, "额度已用完")
				rc.sawNonRateCooldown = true
				// 后台任务在库内新副本上跑：relay 的账号快照被本请求的 503 提示
				// 扫描无锁读取，共享实例就地写（setQuota/setRuntime）会与之竞态。
				// 走节流+单飞版本：并发请求同时撞上同一耗尽账号时只拉一次 billing
				exhaustedID := a.ID
				z.goBackground("quota-refresh", func() {
					fresh, err := z.db.GetAccount(exhaustedID)
					if err != nil {
						return
					}
					z.RefreshAccountQuotaThrottled(fresh)
				})
				// 自动重置策略（默认关闭）：耗尽且自然窗口等待超阈值时才消耗重置
				z.goBackground("auto-reset", func() {
					fresh, err := z.db.GetAccount(exhaustedID)
					if err != nil {
						return
					}
					z.MaybeAutoReset(fresh, "relay")
				})
				return outcomeNextAccount
			}

			// 其它上游错误：按协议回传客户端
			z.pool.MarkFailed(a, fmt.Sprintf("上游错误 HTTP %d", resp.StatusCode))
			z.recordUsage(a, r, payload, resp.StatusCode, start, 0, nil, rc.clientStream)
			writeUpstreamErrorForProto(w, resp, text, rc.proto)
			// 终态失败解绑粘滞：否则粘滞命中+TTL 刷新会把会话域钉死在
			// 持续报错的账号上，failover 永远轮不到
			z.pool.ForgetSticky(rc.sessionKey())
			return outcomeUpstreamError
		}

		// ---- 成功 ----
		if channel == ChannelPaid {
			z.pool.MarkPaidUsed(a)
		} else {
			z.pool.MarkUsed(a)
		}
		// 同耗尽路径：后台刷新用库内新副本，不在 relay 快照实例上就地写
		successID := a.ID
		z.goBackground("relay-success-quota", func() {
			fresh, err := z.db.GetAccount(successID)
			if err != nil {
				return
			}
			z.RefreshAccountQuotaThrottled(fresh)
		})
		log.Printf("[relay] account %s success via %s [%s] (HTTP %d)", a.DisplayNameOrEmail(), pathLabel, channel, resp.StatusCode)

		contentType := resp.Header.Get("Content-Type")
		isStream := strings.Contains(contentType, "text/event-stream")
		// 2xx 但非 JSON/SSE（WAF 挑战页/登录页 HTML）：按上游错误处理，不算成功
		if !isStream && !strings.Contains(contentType, "json") {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			z.markChannelFailure(rc, a, channel, fmt.Sprintf("上游返回非 JSON 内容（%s）", truncate(contentType, 60)), 120)
			writeUpstreamErrorForProto(w, resp, string(body), rc.proto)
			return outcomeUpstreamError
		}

		if !isStream {
			// 非流式响应体总读取时限（红队 F3）：客户端对非流式无超时，慢滴上游
			// 原本可以无限占住账号并发槽。流式不受此限（客户端 600s 空闲超时兜底）
			resp.Body = newTotalDeadlineBody(resp.Body, nonStreamBodyReadDeadline)
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			if readErr != nil {
				// 读取中断/超时：截断的 body 不得洗成成功响应（原代码忽略
				// readErr，半截 JSON 会原样下传给客户端）
				log.Printf("[relay] account %s non-stream body read failed: %v", a.DisplayNameOrEmail(), readErr)
				z.markChannelFailure(rc, a, channel, "上游响应读取中断或超时", 60)
				z.recordUsage(a, r, payload, http.StatusBadGateway, start, 0, nil, rc.clientStream)
				writeAPIError(w, http.StatusBadGateway, "upstream response read interrupted or timed out")
				return outcomeUpstreamError
			}
			// 撞到 64MB 上限（LimitReader EOF 与真实 EOF 无法区分）按失败处理：
			// 静默截断会把截在半截的 input_json_delta 洗成"成功的空参 tool_use"
			if len(body) == 64<<20 {
				z.pool.MarkFailed(a, "上游响应超过 64MB 上限")
				z.recordUsage(a, r, payload, http.StatusBadGateway, start, 0, nil, rc.clientStream)
				writeAPIError(w, http.StatusBadGateway, "upstream response exceeds the 64MB limit")
				return outcomeUpstreamError
			}
			// 2xx + JSON 但不是 message（上游内联错误信封，SSE 路径已证实存在）：
			// 不得洗成"成功空响应"，按上游错误处理
			if isErrorEnvelope(body) {
				z.pool.MarkFailed(a, "上游 2xx 内联错误信封")
				z.recordUsage(a, r, payload, http.StatusBadGateway, start, 0, nil, rc.clientStream)
				writeUpstreamErrorForProto(w, resp, string(body), rc.proto)
				// 终态失败解绑粘滞（与下方 4xx/5xx 分支同规）：MarkFailed 不改
				// 可选状态，粘滞命中会把会话域钉死在持续吐错误信封的账号上
				z.pool.ForgetSticky(rc.sessionKey())
				return outcomeUpstreamError
			}
			usage := parseAnthropicUsageJSON(body)
			// 客户端遥测按 x-request-id → request-id 顺序读取（runner-telemetry）：
			// 与流式透传、错误路径同规转发
			for _, k := range []string{"x-request-id", "request-id"} {
				if v := resp.Header.Get(k); v != "" {
					w.Header().Set(k, v)
				}
			}
			z.recordUsage(a, r, payload, resp.StatusCode, start, 0, usage, rc.clientStream)
			writeProtocolResponse(w, rc, resp.StatusCode, contentType, body, usage)
			return outcomeWritten
		}

		streamProtocolResponse(w, rc, resp, a, r, payload, z, start)
		return outcomeWritten
	}
	return outcomeCaptchaRejected // 验证码重试次数用尽
}

// connFailReason 连接失败的客户端可见原因：经出口代理时只说明代理不可达，
// 不泄露代理地址/端口（失败明细会拼进 503/529 body 发给命名 Key 持有方，
// 可能是第三方）；直连错误的 host 本就是公开的上游地址，可保留。
// 完整错误由调用方写服务端日志。
func connFailReason(proxyURL string, err error) string {
	if proxyURL != "" {
		return "连接失败（出口代理不可达）"
	}
	return "连接失败: " + truncate(err.Error(), 120)
}

// redactProxyURL 代理 URL 进日志前抹掉内嵌凭据（ProxyURLForNode 会拼
// user:pass@ 形态；round-8 自查：connect-failure 日志此前泄露代理密码）
func redactProxyURL(raw string) string {
	if raw == "" {
		return "direct"
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		if at := strings.Index(raw[i+3:], "@"); at >= 0 {
			return raw[:i+3] + raw[i+4+at:]
		}
	}
	return raw
}

// nonStreamBodyReadDeadline 非流式响应体总读取时限。var 便于测试缩短。
var nonStreamBodyReadDeadline = 15 * time.Minute

// totalDeadlineBody 非流式响应体总时限包装：deadline 到点关闭底层连接，
// 阻塞中的 Read 随即报错返回
type totalDeadlineBody struct {
	io.ReadCloser
	stop func()
}

func newTotalDeadlineBody(rc io.ReadCloser, d time.Duration) io.ReadCloser {
	if d <= 0 {
		return rc
	}
	timer := time.AfterFunc(d, func() { rc.Close() })
	return totalDeadlineBody{ReadCloser: rc, stop: func() { timer.Stop() }}
}

func (b totalDeadlineBody) Close() error {
	b.stop()
	return b.ReadCloser.Close()
}

// markChannelFailure 受限冷却按通道落位：免费侧走账号 status/cooling_until，
// 付费侧走 paid_cooling_until，两侧互不牵连。调用点均为非限流类故障
// （连接失败/3xx/非 JSON），计入混合故障信号
func (z *ZCodeAPI) markChannelFailure(rc *relayCtx, a *Account, channel, reason string, seconds int) {
	if rc != nil {
		rc.sawNonRateCooldown = true
	}
	if channel == ChannelPaid {
		z.pool.MarkPaidCooling(a, reason, seconds)
		return
	}
	z.pool.MarkCooling(a, reason, seconds)
}

// isErrorEnvelope 识别 2xx JSON body 里的内联错误信封：
// {"type":"error",...}（Anthropic 风格）或 {"error":...} / {"code":!=0,...}（网关/上游信封）。
// 字段用 RawMessage 接收：某字段类型不符（如 "error":"rate limited" 字符串形态）
// 不得让整封信逃过检测——守卫的目的就是对不可信上游形状 fail closed。
func isErrorEnvelope(body []byte) bool {
	var v struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
		Code  json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		// 整体不是 JSON 对象：按非信封处理（HTML 等由 Content-Type 守卫负责）
		return false
	}
	if v.Type == "error" {
		return true
	}
	if len(v.Error) > 0 && string(v.Error) != "null" {
		return true
	}
	if len(v.Code) > 0 && string(v.Code) != "null" {
		var n float64
		if json.Unmarshal(v.Code, &n) != nil {
			return true // 非数值 code（字符串形态）按错误信封处理
		}
		if n != 0 && n != 200 {
			return true // code 200 是成功封套（与 zcode_api 的解析同规），不算错误
		}
	}
	return false
}

// buildUpstreamRequest 组装上游 URL 与请求头（agent.py build_request + zcode-switch 身份头合并）
func (z *ZCodeAPI) buildUpstreamRequest(a *Account, verifyParam, region string, useFallback bool, r *http.Request, stream bool) (string, map[string]string) {
	up := z.cfg.GetUpstream()
	var urlStr string
	headers := map[string]string{}

	if useFallback && a.APIKey != "" {
		// 回退端点按 provider 选择：bigmodel 的 key 只能发 bigmodel 通道
		if a.Provider == "bigmodel" {
			urlStr = up.Bigmodel
		} else {
			urlStr = up.ZaiFallback
		}
		headers["x-api-key"] = a.APIKey
	} else if a.ZCodeJWT != "" && !useFallback {
		if a.Provider == "bigmodel" {
			urlStr = up.Bigmodel
		} else {
			urlStr = up.Zai
		}
		headers["Authorization"] = "Bearer " + a.ZCodeJWT
	} else if a.APIKey != "" {
		urlStr = up.ZaiFallback
		headers["x-api-key"] = a.APIKey
	} else {
		urlStr = up.Zai
	}
	// 服务端可控的端点路由表（agent/configs proxyEndpoint.mapping，fail-open）
	urlStr = z.routing.Resolve(urlStr)

	// 客户端身份头（与桌面端一致）
	id := NewClientIdentity(z.appVersion, a.DeviceMid)
	for k, v := range ZaiClientHeaders(id) {
		headers[k] = v
	}
	headers["content-type"] = "application/json"
	if stream {
		headers["accept"] = "text/event-stream"
	} else {
		headers["accept"] = "application/json"
	}
	headers["anthropic-version"] = anthropicVersionH
	headers["X-ZCode-Agent"] = "glm"
	if verifyParam != "" {
		headers["X-Aliyun-Captcha-Verify-Param"] = verifyParam
		if region != "" {
			headers["X-Aliyun-Captcha-Verify-Region"] = region
		}
	}

	// 白名单透传客户端 header
	forwardSet := map[string]bool{
		"accept-language": true, "cache-control": true, "anthropic-beta": true,
		"anthropic-dangerous-direct-browser-access": true, "traceparent": true,
		"tracestate": true, "x-client-request-id": true,
	}
	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if forwardSet[lk] || strings.HasPrefix(lk, "x-stainless-") {
			if len(vals) > 0 && len(vals[0]) <= 4096 {
				headers[lk] = vals[0]
			}
		}
	}
	return urlStr, headers
}

// DisplayNameOrEmail 账号展示名
func (a *Account) DisplayNameOrEmail() string {
	return firstNonEmpty(a.DisplayName, a.Email, a.UserID)
}

// ---- 错误识别（gateway.py 移植）----

// rcModel 从请求体提取模型名（日志用）
func rcModel(payload []byte) string {
	var b struct {
		Model string `json:"model"`
	}
	json.Unmarshal(payload, &b)
	return b.Model
}

// isCloudflareChallenge 判定是否 Cloudflare/WAF 人机挑战页
// （cf-mitigated 头或挑战页指纹）。命中说明是出口 IP/风控档位问题：
// 与账号凭证无关，也与阿里云验证码无关——调用方既不能据此判死账号
// （401/403 凭证分支会误杀），也不能当验证码被拒触发重解。
func isCloudflareChallenge(header http.Header, text string) bool {
	if header.Get("Cf-Mitigated") == "challenge" {
		return true
	}
	low := strings.ToLower(text)
	for _, m := range []string{
		"just a moment", "attention required", "checking your browser",
		"verifying you are human", "cf-challenge", "challenge-platform",
		"_cf_chl_opt", "cf-browser-verification", "cf-error-details",
	} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func isCaptchaError(text string) bool {
	low := strings.ToLower(text)
	for _, m := range []string{"captcha", "verify token", "verify failed", "verifycode", "human verification",
		"人机验证", "请完成验证", "安全验证"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func isExhaustedError(statusCode int, text string) bool {
	if statusCode == 402 {
		return true
	}
	low := strings.ToLower(text)
	for _, m := range []string{
		"insufficient balance", "insufficient funds", "no resource package",
		"resource package exhausted", "quota exceeded", "余额不足", "额度已用完",
	} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// isRiskBlocked 风控拦截识别（code 3012 / unusual activity）：账号级临时封禁，冷却处理
func isRiskBlocked(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "unusual activity") || strings.Contains(low, `"code":3012`)
}

// ---- 请求体处理 ----

func readJSONBody(r *http.Request) (map[string]interface{}, *errorResponse) {
	if r.ContentLength > maxRequestBytes {
		return nil, &errorResponse{status: http.StatusRequestEntityTooLarge, msg: "请求体过大"}
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		return nil, &errorResponse{status: http.StatusBadRequest, msg: "读取请求体失败"}
	}
	if len(data) > maxRequestBytes {
		return nil, &errorResponse{status: http.StatusRequestEntityTooLarge, msg: "请求体过大"}
	}
	var v map[string]interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, &errorResponse{status: http.StatusBadRequest, msg: "请求体不是合法 JSON"}
	}
	return v, nil
}

type errorResponse struct {
	status int
	msg    string
}

func (e *errorResponse) Write(w http.ResponseWriter) {
	writeAPIError(w, e.status, e.msg)
}

func detectProvider(body map[string]interface{}, h http.Header) string {
	model, _ := body["model"].(string)
	if strings.HasPrefix(model, "bigmodel/") || h.Get("x-provider") == "bigmodel" {
		return "bigmodel"
	}
	return "zai"
}

// normalizeBody 模型名规范化 + 默认 max_tokens + GLM-5.3 强制思考 + content 桥接
func normalizeBody(body map[string]interface{}, z *ZCodeAPI) error {
	model, _ := body["model"].(string)
	if strings.Contains(model, "/") {
		parts := strings.SplitN(model, "/", 2)
		model = parts[1]
	}
	model = strings.TrimSpace(model)
	if official, ok := modelNameMap[strings.ToLower(model)]; ok {
		model = official
	} else {
		for _, m := range z.cfg.GetModels() {
			if strings.EqualFold(m, model) {
				model = m
				break
			}
		}
	}
	body["model"] = model

	if _, ok := body["max_tokens"]; !ok {
		body["max_tokens"] = float64(4096)
	}
	fixThinking(body)
	applyPromptCacheBreakpoint(body, z)

	// string content → [{type:text,text:...}]
	if msgs, ok := body["messages"].([]interface{}); ok {
		for i, m := range msgs {
			mm, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			if s, ok := mm["content"].(string); ok {
				nm := map[string]interface{}{
					"content": []interface{}{map[string]interface{}{"type": "text", "text": s}},
				}
				for k, v := range mm {
					if k != "content" {
						nm[k] = v
					}
				}
				msgs[i] = nm
			}
		}
		body["messages"] = msgs
	}
	return nil
}

// applyPromptCacheBreakpoint 系统提示词缓存断点（可选，设置 prompt_cache_breakpoint=1 开启）：
// 给 system 的最后一个块标记 cache_control: ephemeral，命中后上游缓存计费按命中价。
// 默认关闭：上游对 cache_control 的接受度未在所有通道实测，出问题时一键可关。
func applyPromptCacheBreakpoint(body map[string]interface{}, z *ZCodeAPI) {
	if v, _ := z.db.GetSetting("prompt_cache_breakpoint"); v != "1" {
		return
	}
	sys, ok := body["system"]
	if !ok || sys == nil {
		return
	}
	breakpoint := map[string]interface{}{"type": "ephemeral"}
	switch s := sys.(type) {
	case string:
		if s == "" {
			return
		}
		body["system"] = []interface{}{map[string]interface{}{
			"type": "text", "text": s, "cache_control": breakpoint,
		}}
	case []interface{}:
		if len(s) == 0 {
			return
		}
		if last, ok := s[len(s)-1].(map[string]interface{}); ok {
			last["cache_control"] = breakpoint
		}
	}
}

// fixThinking GLM-5.3 思考模式归一化为上游现行 wire 格式（对齐 zai-org/ZCode 3.14.x）：
// 思考开启 → {thinking:{type:"adaptive"}, output_config:{effort:"low"|"high"|"max"}}；
// 未请求思考 → {thinking:{type:"disabled"}}。上游不再接受 budget_tokens / reasoning_effort。
func fixThinking(body map[string]interface{}) {
	model, _ := body["model"].(string)
	if !strings.Contains(model, "5.3") {
		// 上游不认识 reasoning_effort：非 5.3 模型直接丢弃，避免整单被参数校验拒绝
		delete(body, "reasoning_effort")
		return
	}
	effort := ""
	if oc, ok := body["output_config"].(map[string]interface{}); ok {
		// 3.14 原生客户端已发 output_config.effort：显式请求优先保留，不得静默改档
		if e, ok := oc["effort"].(string); ok {
			effort = normalizeEffort(e)
		}
	}
	if effort == "" {
		if e, ok := body["reasoning_effort"].(string); ok {
			effort = normalizeEffort(e)
		}
	}
	thinking, _ := body["thinking"].(map[string]interface{})
	if thinking != nil {
		if t, _ := thinking["type"].(string); t == "disabled" {
			body["thinking"] = map[string]interface{}{"type": "disabled"}
			delete(body, "reasoning_effort")
			// 与 adaptive 分支同规：仅剔除 effort 本身，保留 format/task_budget
			//（结构化输出与思考开关正交，整体 delete 会静默丢 schema，轮 10 F8）
			if oc, ok := body["output_config"].(map[string]interface{}); ok {
				rest := map[string]interface{}{}
				for k, v := range oc {
					if k != "effort" {
						rest[k] = v
					}
				}
				if len(rest) > 0 {
					body["output_config"] = rest
				} else {
					delete(body, "output_config")
				}
			}
			return
		}
		if effort == "" {
			if b, ok := thinking["budget_tokens"].(float64); ok {
				effort = effortFromBudget(int(b))
			}
		}
	}
	if effort == "" {
		effort = "high"
	}
	oc := map[string]interface{}{"effort": effort}
	// 保留 output_config 的其余键：ZCode 3.14 SDK 还会挂 format.json_schema
	//（结构化输出）/ task_budget；整体替换会静默丢功能（评审轮 3）
	if prev, ok := body["output_config"].(map[string]interface{}); ok {
		for k, v := range prev {
			if k != "effort" {
				oc[k] = v
			}
		}
	}
	body["thinking"] = map[string]interface{}{"type": "adaptive"}
	body["output_config"] = oc
	delete(body, "reasoning_effort")
}

func normalizeEffort(e string) string {
	switch e {
	case "low", "minimal":
		return "low"
	case "max":
		return "max"
	default: // medium/high 等归并为 high
		return "high"
	}
}

func effortFromBudget(budget int) string {
	switch {
	case budget >= 32768:
		return "max"
	case budget >= 4096:
		return "high"
	default:
		return "low"
	}
}

func validateMessagesBody(ctx context.Context, body map[string]interface{}) error {
	inline := newImageInlineBudget() // 单请求 URL 图片内联额度 + 去重（每次调用即一个请求）
	model, ok := body["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return fmt.Errorf("model must be a non-empty string")
	}
	if len(model) > 200 {
		return fmt.Errorf("model is too long")
	}
	msgs, ok := body["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		return fmt.Errorf("messages must contain at least one message")
	}
	if len(msgs) > 1000 {
		return fmt.Errorf("messages contains too many items")
	}
	// tool_use/tool_result 配对预检：孤儿 tool_result 上游必 400，且会把
	// 健康账号计一次 MarkFailed——本地拒绝并给出准确原因
	declaredToolUses := map[string]int{} // id → 首次出现的 message 下标
	for i, m := range msgs {
		mm, _ := m.(map[string]interface{})
		if mm == nil {
			continue
		}
		if role, _ := mm["role"].(string); role != "assistant" {
			continue
		}
		// asIfaceSlice 兜底 []map[string]interface{}：OpenAI 转换路径产出的
		// 就是该具体类型，纯 []interface{} 断言会漏扫（第二遍主循环有兜底，
		// 预扫描没有——两处必须同构）
		if blocks := asIfaceSlice(mm["content"]); blocks != nil {
			for _, b := range blocks {
				bm, ok := b.(map[string]interface{})
				if !ok {
					continue
				}
				if t, _ := bm["type"].(string); t == "tool_use" {
					if id, _ := bm["id"].(string); id != "" {
						if _, seen := declaredToolUses[id]; !seen {
							declaredToolUses[id] = i
						}
					}
				}
			}
		}
	}
	referencedToolUses := map[string]bool{}
	for _, m := range msgs {
		mm, _ := m.(map[string]interface{})
		if mm == nil {
			continue
		}
		if blocks := asIfaceSlice(mm["content"]); blocks != nil {
			for _, b := range blocks {
				bm, ok := b.(map[string]interface{})
				if !ok {
					continue
				}
				if t, _ := bm["type"].(string); t == "tool_result" {
					if id, _ := bm["tool_use_id"].(string); id != "" {
						referencedToolUses[id] = true
					}
				}
			}
		}
	}
	for i, m := range msgs {
		mm, ok := m.(map[string]interface{})
		if !ok {
			return fmt.Errorf("messages[%d] must be an object", i)
		}
		role, _ := mm["role"].(string)
		// system 放行：zai-org/ZCode 允许 mid-conversation system 消息（硬编码
		// allowSystemInMessages + anthropic-beta: mid-conversation-system-*），
		// 一律 400 会把合法客户端流量打成不可重试的 InvalidModelRequest。
		// 原样透传，由上游判定；仍拒绝其它未知 role
		if role != "user" && role != "assistant" && role != "system" {
			return fmt.Errorf("messages[%d].role must be user, assistant or system", i)
		}
		switch c := mm["content"].(type) {
		case string:
			if len(c) > 2_000_000 {
				return fmt.Errorf("messages[%d].content is too long", i)
			}
		case nil:
			return fmt.Errorf("messages[%d].content must be string or array", i)
		default:
			// 反射兜底：转换器可能产出 []map[string]interface{} 等具体切片类型
			rv := reflect.ValueOf(c)
			if rv.Kind() != reflect.Slice {
				return fmt.Errorf("messages[%d].content must be string or array", i)
			}
			if rv.Len() > 2_000_000 {
				return fmt.Errorf("messages[%d].content is too long", i)
			}
			if rv.Len() == 0 {
				// Anthropic 契约：唯一允许空 content 的是"最后一条 assistant 消息"
				//（从零 prefill 的标准形状），其余一律拒绝
				if !(i == len(msgs)-1 && role == "assistant") {
					return fmt.Errorf("messages[%d].content must not be empty", i)
				}
			}
			for j := 0; j < rv.Len(); j++ {
				bm, ok := rv.Index(j).Interface().(map[string]interface{})
				if !ok {
					return fmt.Errorf("messages[%d].content[%d] must be an object", i, j)
				}
				btype, _ := bm["type"].(string)
				if btype == "" {
					return fmt.Errorf("messages[%d].content[%d] must have a type", i, j)
				}
				// 上游 schema 逐类校验：这些形状本地放行只会换来上游 400 +
				// 账号无谓计一次失败（空 text 例外同上：仅末条 assistant 放行）
				switch btype {
				case "text":
					// 长度与 string content 同规（normalizeBody 已把 string 桥接成
					// text 块，2M 上限不能只挡桥接前的形态）；超长块本地拒绝，
					// 免得 marshal+转发后才被上游 400 并白计一次账号失败
					if txt, _ := bm["text"].(string); len(txt) > 2_000_000 {
						return fmt.Errorf("messages[%d].content[%d]: text block is too long", i, j)
					}
					if t, _ := bm["text"].(string); t == "" {
						if !(i == len(msgs)-1 && role == "assistant") {
							return fmt.Errorf("messages[%d].content[%d]: text block must contain non-empty text", i, j)
						}
					}
				case "tool_use":
					if id, _ := bm["id"].(string); id == "" {
						return fmt.Errorf("messages[%d].content[%d]: tool_use block must have an id", i, j)
					} else if _, seen := declaredToolUses[id]; !seen {
						declaredToolUses[id] = i
					}
					if name, _ := bm["name"].(string); name == "" {
						return fmt.Errorf("messages[%d].content[%d]: tool_use block must have a name", i, j)
					}
				case "tool_result":
					tid, _ := bm["tool_use_id"].(string)
					if tid == "" {
						return fmt.Errorf("messages[%d].content[%d]: tool_result block must have a tool_use_id", i, j)
					}
					if _, ok := declaredToolUses[tid]; !ok {
						return fmt.Errorf("messages[%d].content[%d]: tool_result references unknown tool_use_id %q (dropped assistant turn?)", i, j, tid)
					}
				case "image":
					// 上游只接受 base64 source。url 形态改为受控抓取后内联
					// （image_fetch.go：SSRF 防护 + 限长限时 + content-type 白名单），
					// 抓取失败 fail-closed 回 400，与缺失/null/非对象 source 同规——
					// 放行任何不完整形状只会换来上游 400 + 健康账号白计一次 MarkFailed
					src, _ := bm["source"].(map[string]interface{})
					st, _ := src["type"].(string)
					data, _ := src["data"].(string)
					if st == "url" {
					u, _ := src["url"].(string)
					// 额度按出现处计（去重只省网络抓取——红队 F4：同一 URL
					// 重复 N 次不得绕过上限在 marshal 时放大数 GB）
					if inline.count >= maxInlineImagesPerRequest {
						return fmt.Errorf("messages[%d].content[%d]: too many url images in one request (limit %d)", i, j, maxInlineImagesPerRequest)
					}
					inl, cached := inline.cache[u]
					if !cached {
						var ferr error
						if inl, ferr = fetchImageAsBase64(ctx, u); ferr != nil {
							return fmt.Errorf("messages[%d].content[%d]: image url fetch failed: %v", i, j, ferr)
						}
					}
					inline.count++
					inline.bytes += len(inl.data)
					if inline.bytes > maxInlineBase64BytesPerRequest {
						return fmt.Errorf("messages[%d].content[%d]: inlined image payload exceeds %dMB per request", i, j, maxInlineBase64BytesPerRequest>>20)
					}
					if !cached {
						inline.cache[u] = inl
					}
					bm["source"] = map[string]interface{}{"type": "base64", "media_type": inl.mediaType, "data": inl.data}
					continue
				}
					if src == nil || data == "" {
						return fmt.Errorf("messages[%d].content[%d]: image block must contain base64 data (url sources are not accepted by the upstream)", i, j)
					}
				}
			}
		}
	}
	// 反向配对：tool_use 无任何 tool_result 引用 → 上游 400 + 账号计失败。
	// 末条 assistant 消息例外（prefill 到工具调用的合法形状，模型从这里继续）
	for id, msgIdx := range declaredToolUses {
		if referencedToolUses[id] {
			continue
		}
		if msgIdx == len(msgs)-1 {
			continue
		}
		return fmt.Errorf("messages[%d].content contains tool_use %q with no matching tool_result (dropped tool turn?)", msgIdx, id)
	}
	if mt, ok := body["max_tokens"]; ok {
		switch v := mt.(type) {
		case float64:
			// 分数 token 数上游会拒绝（非整数 JSON）：按无效请求处理
			if v != float64(int64(v)) || v < 1 || v > 1_000_000 {
				return fmt.Errorf("max_tokens must be an integer between 1 and 1000000")
			}
		default:
			return fmt.Errorf("max_tokens must be a number")
		}
	}
	if s, ok := body["stream"]; ok {
		if _, isBool := s.(bool); !isBool {
			return fmt.Errorf("stream must be a boolean")
		}
	}
	return nil
}

// dedup 去重保序
func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// writeUpstreamErrorForProto 上游错误按客户端协议回传
func writeUpstreamErrorForProto(w http.ResponseWriter, resp *http.Response, text string, proto protocol) {
	// 客户端状态码：真实上游 4xx/5xx 透传；2xx/3xx（WAF 挑战页等非错误内容）一律按 502 回传
	status := resp.StatusCode
	if status < 400 {
		status = http.StatusBadGateway
	}
	if proto != protocolAnthropic {
		// OpenAI 风格错误
		msg := "upstream error"
		var payload map[string]interface{}
		if json.Unmarshal([]byte(text), &payload) == nil {
			if e, ok := payload["error"].(map[string]interface{}); ok {
				if m, ok := e["message"].(string); ok && m != "" {
					msg = m
				}
			}
		}
		writeJSON(w, status, map[string]interface{}{
			"error": map[string]interface{}{"message": truncate(msg, 500), "type": "api_error", "code": status},
		})
		return
	}
	for _, k := range []string{"retry-after", "x-request-id", "request-id"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	var v map[string]interface{}
	if json.Unmarshal([]byte(text), &v) == nil {
		// 上游错误体未必带顶层 type:"error"（bigmodel {"error":{...}} 等）：
		// 客户端 schema 要求该键，缺失时补上再回传，保留上游其余字段原样
		if s, ok := v["type"].(string); !ok || s != "error" {
			v["type"] = "error"
			if b, merr := json.Marshal(v); merr == nil {
				w.Write(b)
				return
			}
		}
		w.Write([]byte(text))
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type":  "error",
		"error": map[string]string{"message": truncate(text, 500), "type": "upstream_error"},
	})
}

// RefreshAccountQuotaThrottled 成功请求后的即时额度刷新（30s 节流；
// 进程内单飞由 RefreshAccountQuota 统一把守，所有入口共享）
func (z *ZCodeAPI) RefreshAccountQuotaThrottled(a *Account) {
	if a.Provider != "zai" || a.ZCodeJWT == "" {
		return
	}
	if time.Now().Unix()-a.lastCheckedAt() < 30 {
		return
	}
	// lastCheckedAt 来自 ListAccounts 的库内快照，N 个并发请求可能同时选中同一
	// 账号副本并通过节流检查；RefreshAccountQuota 的单飞把它们收敛为一次拉取
	if err := z.RefreshAccountQuota(a); err != nil && !errors.Is(err, errRefreshInFlight) {
		log.Printf("[quota] throttled refresh %s: %v", a.Email, err)
	}
}

// recordUsage 落 usage_records；clientStream 为客户端真实请求模式（非上游内部流式标志）
func (z *ZCodeAPI) recordUsage(a *Account, r *http.Request, payload []byte, statusCode int, start time.Time, ttftMs int, usage *StreamUsage, clientStream bool) {
	var body map[string]interface{}
	json.Unmarshal(payload, &body)
	model, _ := body["model"].(string)
	rec := &UsageRecord{
		AccountID:  a.ID,
		Email:      firstNonEmpty(a.Email, a.DisplayName),
		Model:      model,
		Stream:     clientStream,
		StatusCode: statusCode,
		DurationMs: int(time.Since(start).Milliseconds()),
		TtftMs:     ttftMs,
		Channel:    a.usageChannelName(), // free/paid 通道归因（付费通道按量计费，日限额依赖）
	}
	if usage != nil {
		rec.PromptTokens = usage.InputTokens
		rec.CompletionTokens = usage.OutputTokens
		rec.TotalTokens = usage.InputTokens + usage.OutputTokens
		rec.CacheReadTokens = usage.CacheReadTokens
		rec.CacheCreationTokens = usage.CacheCreationTokens
	}
	// 命名网关 Key 归因（R1）：记录到 usage 并回写 Key 配额消耗
	if gk := gatewayKeyFromCtx(r.Context()); gk != nil {
		rec.GatewayKeyID = gk.ID
		rec.KeyName = gk.Name
	}
	if err := z.db.InsertUsageRecord(rec); err != nil {
		log.Printf("[usage] insert: %v", err)
	}
	if rec.GatewayKeyID > 0 {
		// 配额按计费口径折算（含缓存 token，见 gatewayKeyQuotaCharge），
		// usage_records 落库口径不变
		z.db.BumpGatewayKeyUsage(rec.GatewayKeyID, gatewayKeyQuotaCharge(usage))
	}
}
