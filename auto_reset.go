package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- 自动重置策略（可配置阈值；默认关闭）----
// 只在账号配额耗尽时评估，且全部条件满足才消耗重置机会：
//   1. auto_reset_enabled = 1（默认 0：重置永远只手动）
//   2. 有可用槽位（five_hour 优先，与手动流程一致）
//   3. 自然窗口重置的等待时间 > 阈值——等 5 分钟就自然恢复时绝不动用重置：
//      auto_reset_min_wait_minutes   FIVE_HOUR 阈值（分钟，默认 60）
//      auto_reset_min_wait_week_hours WEEK 阈值（小时，默认 24）
//      等待时间未知（monitor 通道未给出 next_reset）视为足够远——
//      看不见窗口时按需恢复，符合"耗尽即取用"的预期
// 防抖：同账号自动评估最小间隔 10 分钟，失败/跳过都不因每条 402 反复打状态接口。
//
// ---- 临期槽位 use-it-or-lose-it ----
// 重置机会槽位自带 expire_at（上游毫秒已实证——ZCode 客户端 app.asar 与
// Date.now() 直接比较；经 FetchResetStatus 归一为秒），到期未用即作废。
// 上面的阈值保留策略在"槽位先于下次耗尽到期"时会把槽位一直保留到作废——保留到
// 作废不如到期前花掉。临期消耗由独立开关 auto_reset_expiry_enabled 控制
// （默认关：自动动用重置机会整体是 opt-in，与本文件顶部的阈值主开关同款默认关
// 语义；开启后独立于主开关生效），窗口为
// auto_reset_expiry_spend_minutes（默认 60，0=关闭）：无论账号是否耗尽、自然
// 恢复多近，都消耗临期槽位——剩余额度在自然重置时不结转，临期消耗至少把
// (100-剩余)% 的恢复价值拿到手。非耗尽账号没有 402 触发点，由 reset/status
// 同步路径（每账号 ≤10 分钟一次）代为评估；refreshAll 会跳过冷却中的账号，
// 冷却账号的心跳由调度器 SweepExpiringResetsTick 补上——窗口设在到期前一小时
// 也给失败重试留出多次机会。

const autoResetAttemptInterval = 10 * time.Minute

// autoResetExpirySpendDefaultMin 临期消耗窗口默认值（分钟）
const autoResetExpirySpendDefaultMin = 60

// autoResetExpirySpendGap 同账号两次临期消耗之间的最小间隔：
// 防上游 status 短暂滞后（use 已受理但槽位仍显示可用）导致同槽位双花
const autoResetExpirySpendGap = 30 * time.Minute

// autoResetExpiryEnabled 临期槽位自动消耗开关：默认关（显式 "1" 才开）。
// "自动动用重置机会"整体是 opt-in——耗尽阈值路径与临期路径都不该在用户
// 未表态时动槽位。历史版本此处默认开（fail-open），2026-10-09 按用户意图
// 收紧为默认关
func autoResetExpiryEnabled(db *DB) bool {
	v, _ := db.GetSetting("auto_reset_expiry_enabled")
	return v == "1"
}

var autoResetState = struct {
	sync.Mutex
	lastAttempt     map[int64]time.Time
	lastExpirySpend map[int64]time.Time
	// 上一次临期消耗的槽位到期时间：gap 内出现"不同到期时间"的临期槽时，
	// 锁内重拉列表即上游亲证是另一槽，gap 让位（否则第二个临期槽必死在 gap 内）
	lastExpirySpendSlot map[int64]int64
	// 临期心跳路径（冷却账号）每账号上次评估时刻：调度器每分钟 tick，
	// 上游 status 调用须按账号节流（与 resetHistorySyncInterval 同级）
	lastSweep map[int64]time.Time
}{lastAttempt: map[int64]time.Time{}, lastExpirySpend: map[int64]time.Time{}, lastExpirySpendSlot: map[int64]int64{}, lastSweep: map[int64]time.Time{}}

// autoResetShouldSpend 阈值判定（纯函数，供测试）：等待未知 = 花；
// 等待已知且 >= 阈值 = 花；否则等自然恢复
func autoResetShouldSpend(waitKnown bool, waitSeconds int64, thresholdSeconds int64) bool {
	if !waitKnown {
		return true
	}
	return waitSeconds >= thresholdSeconds
}

// autoResetSlotExpiringSoon 返回该组槽位中最早进入临期窗口的到期时间（纯函数，供测试）。
// expire_at<=now（上游残留的已过期数据）不触发——花一个已过期槽位必然失败；
// expire_at==0（上游未给出到期时间）视为无限期，按非临期处理
func autoResetSlotExpiringSoon(now, windowSec int64, slots []ResetSlot) (int64, bool) {
	if windowSec <= 0 {
		return 0, false
	}
	best, ok := int64(0), false
	for _, s := range slots {
		if s.ExpireAt <= now || s.ExpireAt > now+windowSec {
			continue
		}
		if !ok || s.ExpireAt < best {
			best, ok = s.ExpireAt, true
		}
	}
	return best, ok
}

// autoResetWait 从账号额度快照取自然窗口重置等待（秒）；未知返回 ok=false。
// QuotaJSON 由额度刷新 goroutine 持锁改写（setQuota），必须锁内取快照：
// string 头两字非原子，撕裂读取会 segfault 或喂错重置决策
func autoResetWait(a *Account) (int64, bool) {
	a.mu.Lock()
	quotaJSON := a.QuotaJSON
	a.mu.Unlock()
	if quotaJSON == "" {
		return 0, false
	}
	var ov struct {
		NextReset int64 `json:"next_reset"`
	}
	if json.Unmarshal([]byte(quotaJSON), &ov) != nil || ov.NextReset <= 0 {
		return 0, false
	}
	wait := ov.NextReset - time.Now().Unix()
	if wait < 0 {
		wait = 0
	}
	return wait, true
}

// MaybeAutoReset 自动重置评估（非阻塞调用：内部自持锁，失败只记日志）：
// 耗尽阈值消耗（auto_reset_enabled 门控）+ 临期槽位消耗（expiry 开关门控）。
// trigger 仅用于日志定位（relay / quota / manual-check）。
func (z *ZCodeAPI) MaybeAutoReset(a *Account, trigger string) {
	// 两路开关：auto_reset_enabled 门控耗尽阈值消耗（默认关）；
	// auto_reset_expiry_enabled 门控临期消耗（默认关，独立于主开关）
	expiryOn := autoResetExpiryEnabled(z.db)
	autoOn := false
	if v, _ := z.db.GetSetting("auto_reset_enabled"); v == "1" {
		autoOn = true
	}
	if !autoOn && !expiryOn {
		return
	}
	// 无 JWT 的账号（纯 API Key）不可能走 JWT 通道重置：不提前拦下，
	// 每轮评估都会记一条"需要 ZCode JWT"失败日志，永无成功可能
	if jwt, _, _ := a.credentialSnapshot(); jwt == "" {
		return
	}
	// 锁保护读：a 可能正被并发额度刷新 goroutine 改写状态
	if status, _ := a.statusError(); status != StatusExhausted {
		return
	}

	autoResetState.Lock()
	last, hasLast := autoResetState.lastAttempt[a.ID]
	autoResetState.Unlock()
	if hasLast && time.Since(last) < autoResetAttemptInterval {
		// 单飞重试（-retry 后缀）不受他人刚盖的印记阻挡：印记被并发 402
		// 重盖会让延迟重试在 debounce 处静默丢失（耗尽账号不再有下一触发）
		if !strings.HasSuffix(trigger, "-retry") {
			return
		}
		autoResetState.Lock()
		delete(autoResetState.lastAttempt, a.ID)
		autoResetState.Unlock()
	}

	// 与手动重置/领取同一把账号级锁：绝不与面板操作双消耗
	mu := z.claimLockFor(a.ID)
	if !mu.TryLock() {
		return
	}
	defer mu.Unlock()

	// 印记在拿到 claim 锁后才落：锁忙早退不是一次真实评估，先盖印会让一次
	// 锁竞争吃掉整个 10 分钟评估窗口（耗尽账号不再有下一触发，评估永久丢失）
	autoResetState.Lock()
	autoResetState.lastAttempt[a.ID] = time.Now()
	autoResetState.Unlock()

	st, _, _, err := z.FetchResetStatus(a)
	if err != nil {
		log.Printf("[auto-reset] %s status: %v", a.DisplayNameOrEmail(), err)
		return
	}

	resetType := ""
	thresholdSeconds := int64(0)
	switch {
	case len(st.AvailableFiveHourResets) > 0:
		resetType = "FIVE_HOUR"
		mins := settingInt(z.db, "auto_reset_min_wait_minutes", 60)
		thresholdSeconds = int64(mins) * 60
	case len(st.AvailableWeekResets) > 0:
		resetType = "WEEK"
		hours := settingInt(z.db, "auto_reset_min_wait_week_hours", 24)
		thresholdSeconds = int64(hours) * 3600
	default:
		log.Printf("[auto-reset] %s: no reset opportunities left", a.DisplayNameOrEmail())
		return
	}

	wait, waitKnown := autoResetWait(a)
	// 临期槽位压过阈值保留：阈值假设"留着以后耗尽时用更划算"，
	// 但槽位先到期时留 = 作废，花掉才是唯一不掉价值的选项。
	// 窗口 0（开关关）时 autoResetSlotExpiringSoon 恒 false，退回纯阈值判定
	windowSec := int64(0)
	if expiryOn {
		windowSec = int64(settingInt(z.db, "auto_reset_expiry_spend_minutes", autoResetExpirySpendDefaultMin)) * 60
	}
	// 临期判定必须跨两表：five-hour 列表非空时 WEEK 临期槽此前不可见，
	// 阈值保留路径会白等到它过期作废
	now := time.Now().Unix()
	exp5, expiring5 := autoResetSlotExpiringSoon(now, windowSec, st.AvailableFiveHourResets)
	expW, expiringW := autoResetSlotExpiringSoon(now, windowSec, st.AvailableWeekResets)
	var expireAt int64
	expiring := false
	switch {
	case expiring5:
		resetType = "FIVE_HOUR"
		expireAt, expiring = exp5, true
	case expiringW:
		resetType = "WEEK"
		expireAt, expiring = expW, true
	}
	// 阈值消耗只在主开关开时生效；主开关关时仅临期槽位可触发消耗
	if !expiring && !(autoOn && autoResetShouldSpend(waitKnown, wait, thresholdSeconds)) {
		if autoOn {
			log.Printf("[auto-reset] %s: %s window resets naturally in %s (< threshold), keeping reset slot",
				a.DisplayNameOrEmail(), resetType, humanizeWait(wait, waitKnown))
		} else {
			log.Printf("[auto-reset] %s: threshold spend disabled (auto_reset_enabled=0), keeping %s slot",
				a.DisplayNameOrEmail(), resetType)
		}
		return
	}
	detail := "耗尽触发，等待 " + humanizeWait(wait, waitKnown) + " 超过阈值"
	if expiring {
		detail = "槽位临期（" + humanizeWait(expireAt-time.Now().Unix(), true) + " 后到期）"
	}

	// 动笔前以库内最新状态复核：402 触发时 relay 同时拉起配额刷新，
	// 瞬时误报可能已被并发刷新恢复 active——此时绝不消耗重置槽位
	fresh, ferr := z.db.GetAccount(a.ID)
	if ferr != nil {
		log.Printf("[auto-reset] %s re-read: %v", a.DisplayNameOrEmail(), ferr)
		return
	}
	if fresh.Status != StatusExhausted {
		log.Printf("[auto-reset] %s: no longer exhausted (concurrent refresh recovered), keeping reset slot", a.DisplayNameOrEmail())
		return
	}

	// 配额刷新仍在单飞中：排空它再复核。裸返回 = 本次评估永久丢失——
	// 耗尽账号不会再被选中、不会再产生 402 触发，唯一出路是主动重试
	// （复核清掉 debounce 印记，两次上限：刷新单飞有超时，不会无限排队）
	if _, busy := z.quotaRefreshInflight.Load(a.ID); busy {
		if strings.HasSuffix(trigger, "-retry") {
			log.Printf("[auto-reset] %s: refresh still in flight after retry, giving up this episode", a.DisplayNameOrEmail())
			return
		}
		autoResetState.Lock()
		delete(autoResetState.lastAttempt, a.ID)
		autoResetState.Unlock()
		log.Printf("[auto-reset] %s: quota refresh in flight, retrying in 5s", a.DisplayNameOrEmail())
		retry := trigger + "-retry"
		z.goBackground("auto-reset-retry", func() {
			time.Sleep(5 * time.Second)
			z.MaybeAutoReset(a, retry)
		})
		return
	}

	// 临期槽位经共享防双花闸门动笔（同步路径同闸门，见 spendExpiringSlotGuarded）；
	// 阈值路径无临期语义，直接消耗。used/msg/err 由闭包带回走原记录逻辑。
	// 幂等键只在临期路径用确定性键（账号+槽位到期）：阈值路径的 expireAt 恒为
	// 0（无槽位语义），确定性键会退化为 accountID 常量，把同一账号两次合法的
	// 阈值消耗（今天的 five_hour 与下周的 week）撞成同键——上游幂等真生效时
	// 第二次会被误拒。空键回落随机 UUID（UseReset 内）
	idemKey := autoResetIdemKey(expiring, a.ID, expireAt)
	var used bool
	var msg string
	spendOnce := func() error {
		used, _, msg, err = z.UseReset(a, resetType, idemKey)
		if err != nil {
			return err
		}
		if !used {
			return fmt.Errorf("used=false")
		}
		return nil
	}
	if expiring {
		// 冷启动兜底先行：内存无印但账本 gap 内有成功重置（重启清印后的
		// 重放形态）→ 与 slot=0 无归属同款保守拒绝
		if z.expirySpendColdBlocked(a.ID) {
			log.Printf("[auto-reset] %s: expiring slot deferred (recent ledger spend, cold guard)", a.DisplayNameOrEmail())
			return
		}
		if spent, reason := spendExpiringSlotGuarded(a.ID, expireAt, spendOnce); !spent && reason == spendGuardGapRefused {
			// 同步路径 gap 内已消耗同一槽位（上游 status 滞后）：让位，不重复动笔
			log.Printf("[auto-reset] %s: expiring slot deferred (30min same-account spend guard)", a.DisplayNameOrEmail())
			return
		}
	} else {
		// 阈值路径防重放：任一路径 30 分钟内成功消耗过（暖印或冷启动账本）
		// 都暂缓——上游 status 滞后时阈值路径会把幻影槽位当真再花一次。
		// 进入此分支前 expiring 判定已确认窗口内无临期槽，暂缓不会错过
		// 临期消耗（槽位真进窗口时由临期路径接管）
		if z.thresholdSpendDeferred(a.ID) {
			log.Printf("[auto-reset] %s: threshold spend deferred (recent spend within 30min guard)", a.DisplayNameOrEmail())
			return
		}
		spendOnce()
	}
	// UsedAt 精确落本地执行时刻：同步去重优先精确匹配，免去 localtime
	// 墙钟换算（DST 切换时窗口会漂移出重复行）
	record := &ClaimRecord{AccountID: a.ID, Email: a.Email, TaskType: "reset", UsedAt: time.Now().Unix()}
	if err != nil || !used {
		record.Message = fmt.Sprintf("自动重置执行失败(%s): %v %s", resetType, err, msg)
		if errors.Is(err, errResetTransport) {
			// 请求发出但响应未达：上游可能已受理消耗，账本必须如实标注，
			// 否则一条"纯失败"记录会诱导后续把同一槽位再花一次
			record.Message += "；响应未达，槽位可能已被上游消耗"
		}
		z.db.InsertClaimRecord(record)
		z.db.SetAccountClaimResult(a.ID, "配额重置", record.Message)
		log.Printf("[auto-reset] %s: %s", a.DisplayNameOrEmail(), record.Message)
		return
	}
	record.Success = true
	record.PlanName = "配额重置(" + resetType + ")[自动]"
	record.Message = "自动重置成功（" + detail + "），配额已恢复"
	z.db.InsertClaimRecord(record)
	z.db.SetAccountClaimResult(a.ID, record.PlanName, record.Message)
	log.Printf("[auto-reset] %s: quota restored via %s (trigger=%s)", a.DisplayNameOrEmail(), resetType, trigger)
	z.goBackground("auto-reset-quota", func() {
		time.Sleep(2 * time.Second)
		z.RefreshAccountQuota(a)
	})
}

// SweepExpiringResetsTick 调度器每分钟心跳调用：为 refreshAll 跳过的冷却账号
// （cooling_until 在未来）补上临期槽位评估心跳。refreshAll 跳过它们是为省额度
// 接口调用，但 spendExpiringResetForSync 明确接受 cooling 账号——两处资格不
// 一致曾让冷却中的临期槽位只能过期作废。每账号 10 分钟节流 + TryLock，上游
// status 调用频率与同步路径同级；开关关/窗口 0 时整体零开销返回。
// stop 为调度器停机通道：每账号之间探测，停机即刻让路——否则多账号轮询叠加
// 网络超时会拖过 Stop 的 15s 有界等待，账本写库可能撞上已关闭的 db
func (z *ZCodeAPI) SweepExpiringResetsTick(stop <-chan struct{}) {
	if !autoResetExpiryEnabled(z.db) {
		return
	}
	if windowSec := int64(settingInt(z.db, "auto_reset_expiry_spend_minutes", autoResetExpirySpendDefaultMin)) * 60; windowSec <= 0 {
		return
	}
	accounts, err := z.db.ListAccounts("")
	if err != nil {
		return
	}
	now := time.Now()
	for _, a := range accounts {
		select {
		case <-stop:
			return
		default:
		}
		if !a.Enabled || a.Status != StatusCooling || a.CoolingUntil <= now.Unix() {
			continue
		}
		if jwt, _, _ := a.credentialSnapshot(); jwt == "" {
			continue
		}
		autoResetState.Lock()
		last := autoResetState.lastSweep[a.ID]
		autoResetState.Unlock()
		if now.Sub(last) < autoResetAttemptInterval {
			continue
		}
		mu := z.claimLockFor(a.ID)
		if !mu.TryLock() {
			continue
		}
		// 印记在拿到 claim 锁后才落（MaybeAutoReset 同课：锁忙早退不是一次
		// 真实评估，先盖印会让锁竞争吃掉整个节流窗口）
		autoResetState.Lock()
		autoResetState.lastSweep[a.ID] = now
		autoResetState.Unlock()
		z.spendExpiringResetForSync(a, nil)
		mu.Unlock()
	}
}

// settingInt 整数设置读取（非法/缺失回落默认）
func settingInt(db *DB, key string, def int) int {
	if v, _ := db.GetSetting(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

func humanizeWait(waitSeconds int64, known bool) string {
	if !known {
		return "未知"
	}
	if waitSeconds < 90 {
		return strconv.FormatInt(waitSeconds, 10) + "s"
	}
	return strconv.FormatInt((waitSeconds+59)/60, 10) + "m"
}

// spendGuardGapRefused 防双花闸门 gap 拒绝的固定原因串：调用方据此区分
// "拒绝动笔"（不落记录）与"动笔失败"（落失败记录）
const spendGuardGapRefused = "gap-refused"

// markExpirySpend 防双花盖印（无条件跟随每一次成功消耗）：slot=0 表示无归属
// （调用方不知道 expireAt，如手动/阈值路径），闸门谓词对其保守拒绝 gap 内一切
// 临期消耗。必须由 UseReset 的成功路径调用——只靠闸门盖印时，手动消耗 + 上游
// status 滞后会把同一槽位再花一次（评审 F3）。
func markExpirySpend(accountID int64, slot int64) {
	autoResetState.Lock()
	autoResetState.lastExpirySpend[accountID] = time.Now()
	autoResetState.lastExpirySpendSlot[accountID] = slot
	autoResetState.Unlock()
}

// autoResetIdemKey 幂等键选择（纯函数，供测试）：临期路径用确定性键
// "auto-expiry-<账号>-<槽位到期>"，同槽重试/重启重放同键，上游幂等可拦截
// 同槽双花；阈值路径 expireAt 恒 0、无稳定槽位标识，必须返回空键（回落
// 随机 UUID）——常量键会把同一账号先后两次合法阈值消耗误判为重放
func autoResetIdemKey(expiring bool, accountID int64, expireAt int64) string {
	if !expiring {
		return ""
	}
	return fmt.Sprintf("auto-expiry-%d-%d", accountID, expireAt)
}

// thresholdSpendDeferred 阈值路径的防重放闸（G2）：任一路径 30 分钟内成功
// 消耗过重置（暖内存印，或冷启动查账本）即暂缓。上游 status 滞后时，阈值
// 路径会把幻影槽位当真再花一次——重启清空内存印后风险最高
func (z *ZCodeAPI) thresholdSpendDeferred(accountID int64) bool {
	autoResetState.Lock()
	last := autoResetState.lastExpirySpend[accountID]
	autoResetState.Unlock()
	if time.Since(last) < autoResetExpirySpendGap {
		return true
	}
	return z.expirySpendColdBlocked(accountID)
}

// expirySpendColdBlocked 临期消耗的冷启动账本兜底：防双花印本是内存态、进程
// 重启清零——重启恰好落在上游 status 滞后窗口内时，同槽位可能被再花一次。
// 内存无印（冷路径）时查账本：gap 内有过成功重置记录（无论哪条路径，槽位
// 归属未知）按 slot=0 无归属同款语义拒绝；内存有印（热路径）零开销不查库。
// 调用方必须已持该账号的 claim 锁（与闸门同前提），本检查→闸门→动笔全程原子
func (z *ZCodeAPI) expirySpendColdBlocked(accountID int64) bool {
	autoResetState.Lock()
	last := autoResetState.lastExpirySpend[accountID]
	autoResetState.Unlock()
	if !last.IsZero() {
		return false
	}
	ok, err := z.db.HasResetRecordSince(accountID, time.Now().Add(-autoResetExpirySpendGap).Unix())
	return err == nil && ok
}

// spendExpiringSlotGuarded 临期槽位消耗的共享防双花闸门：gap 内同到期槽位拒绝，
// 动笔成功才盖印（lastExpirySpend/lastExpirySpendSlot）。
// MaybeAutoReset（耗尽路径）与 spendExpiringResetForSync（同步路径）都从这里
// 动笔——同一轮配额刷新可能同时拉起两条路径（applyQuotaResult → goBackground
// MaybeAutoReset；refreshFn → SyncResetHistoryFromUpstream → 临期消耗），上游
// status 滞后时第二路径会把下一个稀缺槽位当"仍是旧槽"再花一次。
// 调用方必须已持该账号的 claim 锁（两条路径各自持有），保证检查→动笔→盖印
// 相对另一条路径原子；重启后的冷启动兜底见 expirySpendColdBlocked。
func spendExpiringSlotGuarded(accountID int64, expireAt int64, spend func() error) (spent bool, reason string) {
	autoResetState.Lock()
	last := autoResetState.lastExpirySpend[accountID]
	lastSlot := autoResetState.lastExpirySpendSlot[accountID]
	autoResetState.Unlock()
	// lastSlot == 0 = 无归属盖印（UseReset 对手动/阈值路径的消耗无条件盖印，
	// 归属槽位未知）：保守视为可能同槽，gap 内拒绝一切临期消耗——手动消耗后
	// 上游 status 滞后复述旧槽会双花；代价只是另一真实槽位的临期消耗暂缓 ≤gap
	if time.Since(last) < autoResetExpirySpendGap && (lastSlot == 0 || expireAt == lastSlot) {
		return false, spendGuardGapRefused
	}
	if err := spend(); err != nil {
		return false, err.Error()
	}
	autoResetState.Lock()
	autoResetState.lastExpirySpend[accountID] = time.Now()
	autoResetState.lastExpirySpendSlot[accountID] = expireAt
	autoResetState.Unlock()
	return true, ""
}

// spendExpiringResetForSync 临期槽位消耗（非耗尽路径）：active/cooling/exhausted
// 账号的槽位没有 402 触发点，只能靠周期评估在到期前花掉。两条入口：
//   - SyncResetHistoryFromUpstream（st 为预筛快照）：预筛只为绝大多数无临期槽位
//     的账号省掉第二次 status 请求，真要动笔前必须锁内重拉 status 复核，
//     防止快照滞后期间槽位已被 relay/手动消耗；
//   - SweepExpiringResetsTick（st == nil，冷却账号心跳）：refreshAll 跳过
//     cooling_until 在未来的账号，此处锁内拉取即复核。
//
// 两条入口都要求调用方已持该账号的 claim 锁（互斥手动/relay 路径）。
func (z *ZCodeAPI) spendExpiringResetForSync(a *Account, st *ResetStatus) {
	// 独立开关（默认关）：临期消耗不依赖 auto_reset_enabled 主开关
	if !autoResetExpiryEnabled(z.db) {
		return
	}
	// 无 JWT 的账号重置必然失败：提前拦下，不给日志刷"需要 ZCode JWT"的机会
	if jwt, _, _ := a.credentialSnapshot(); jwt == "" {
		return
	}
	stFreshUnderLock := false
	if st == nil {
		st2, _, _, err := z.FetchResetStatus(a)
		if err != nil {
			return
		}
		st = st2
		stFreshUnderLock = true
	}
	windowSec := int64(settingInt(z.db, "auto_reset_expiry_spend_minutes", autoResetExpirySpendDefaultMin)) * 60
	if windowSec <= 0 {
		return
	}
	now := time.Now().Unix()
	if _, soon5 := autoResetSlotExpiringSoon(now, windowSec, st.AvailableFiveHourResets); !soon5 {
		if _, soonW := autoResetSlotExpiringSoon(now, windowSec, st.AvailableWeekResets); !soonW {
			return
		}
	}
	// 状态门槛：invalid（凭证坏）/ disabled / inactive（无套餐）花重置必然失败或无意义；
	// active / cooling / exhausted 都值得恢复额度
	fresh, err := z.db.GetAccount(a.ID)
	if err != nil || fresh == nil {
		return
	}
	switch fresh.Status {
	case StatusActive, StatusExhausted, StatusCooling:
	default:
		return
	}
	// 配额刷新单飞中不消耗：刷新可能恰好改变额度（与 MaybeAutoReset 同一守卫）
	if _, busy := z.quotaRefreshInflight.Load(a.ID); busy {
		return
	}
	// 锁内复核状态：预筛路径的 st 是锁外快照，动笔前必须重拉复核，防止快照
	// 滞后期间槽位已被 relay/手动消耗；心跳路径的 st 已是锁内新取（调用方
	// 持 claim 锁期间拉取），免二次上游往返
	stFinal := st
	if !stFreshUnderLock {
		st2, _, _, err := z.FetchResetStatus(fresh)
		if err != nil {
			return
		}
		stFinal = st2
	}
	// 决策时钟必须重取：now 采集于预筛，两次 status 之间的网络往返可能跨过
	// 槽位 expire_at——用旧值判定会花在上游已失效的槽上（注定失败还落成功记录）
	now = time.Now().Unix()
	resetType := ""
	var expireAt int64
	if at, soon := autoResetSlotExpiringSoon(now, windowSec, stFinal.AvailableFiveHourResets); soon {
		resetType, expireAt = "FIVE_HOUR", at
	} else if at, soon := autoResetSlotExpiringSoon(now, windowSec, stFinal.AvailableWeekResets); soon {
		resetType, expireAt = "WEEK", at
	} else {
		return
	}
	// 动笔走共享防双花闸门（MaybeAutoReset 同闸门）：30 分钟 gap 防上游列表
	// 滞后双花；锁内重拉列表里出现"不同到期时间"的临期槽 = 上游亲证是另一槽，
	// gap 让位（否则第二个临期槽必死在 gap 内）。useErr/useMsg 由闭包带回走原
	// 失败记录格式；gap 拒绝记一条暂缓日志（真正花掉的那条路径有自己的成功日志）。
	// 确定性幂等键（账号+槽位到期，恒为临期语义）：同槽重试/重启重放同键
	var useErr error
	var useMsg string
	var useOK bool
	if z.expirySpendColdBlocked(fresh.ID) {
		log.Printf("[auto-reset] %s: expiring slot deferred (recent ledger spend, cold guard)", fresh.DisplayNameOrEmail())
		return
	}
	spent, reason := spendExpiringSlotGuarded(fresh.ID, expireAt, func() error {
		useOK, _, useMsg, useErr = z.UseReset(fresh, resetType, autoResetIdemKey(true, fresh.ID, expireAt))
		if useErr != nil {
			return useErr
		}
		if !useOK {
			return fmt.Errorf("used=false")
		}
		return nil
	})
	record := &ClaimRecord{AccountID: fresh.ID, Email: fresh.Email, TaskType: "reset", UsedAt: time.Now().Unix()}
	if !spent {
		switch reason {
		case spendGuardGapRefused:
			log.Printf("[auto-reset] %s: expiring %s slot deferred (30min same-account spend guard)", fresh.DisplayNameOrEmail(), resetType)
		default:
			record.Message = fmt.Sprintf("临期自动重置执行失败(%s): %v %s", resetType, useErr, useMsg)
			if errors.Is(useErr, errResetTransport) {
				// 响应未达：上游可能已受理，账本如实标注（同 MaybeAutoReset）
				record.Message += "；响应未达，槽位可能已被上游消耗"
			}
			z.db.InsertClaimRecord(record)
			log.Printf("[auto-reset] %s: %s", fresh.DisplayNameOrEmail(), record.Message)
		}
		return
	}
	record.Success = true
	record.PlanName = "配额重置(" + resetType + ")[自动-临期]"
	record.Message = fmt.Sprintf("槽位 %s 后到期，临期自动消耗，配额已恢复", humanizeWait(expireAt-now, true))
	z.db.InsertClaimRecord(record)
	z.db.SetAccountClaimResult(fresh.ID, record.PlanName, record.Message)
	log.Printf("[auto-reset] %s: expiring %s slot spent (expires in %s)",
		fresh.DisplayNameOrEmail(), resetType, humanizeWait(expireAt-now, true))
	z.goBackground("auto-reset-expiry-quota", func() {
		time.Sleep(2 * time.Second)
		z.RefreshAccountQuota(fresh)
	})
}
