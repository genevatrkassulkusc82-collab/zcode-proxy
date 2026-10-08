package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
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
// 重置机会槽位自带 expire_at，到期未用即作废。上面的阈值保留策略在"槽位先于
// 下次耗尽到期"时会把槽位一直保留到作废——保留到作废不如到期前花掉。临期消耗
// 由独立开关 auto_reset_expiry_enabled 控制（默认开：到期作废的槽位花了不亏，
// 不依赖上面默认关闭的耗尽阈值主开关），窗口为
// auto_reset_expiry_spend_minutes（默认 60，0=关闭）：无论账号是否耗尽、自然
// 恢复多近，都消耗临期槽位——剩余额度在自然重置时不结转，临期消耗至少把
// (100-剩余)% 的恢复价值拿到手。非耗尽账号没有 402 触发点，由 reset/status
// 同步路径（每账号 ≤10 分钟一次）代为评估——窗口设在到期前一小时也给失败
// 重试留出多次机会。

const autoResetAttemptInterval = 10 * time.Minute

// autoResetExpirySpendDefaultMin 临期消耗窗口默认值（分钟）
const autoResetExpirySpendDefaultMin = 60

// autoResetExpirySpendGap 同账号两次临期消耗之间的最小间隔：
// 防上游 status 短暂滞后（use 已受理但槽位仍显示可用）导致同槽位双花
const autoResetExpirySpendGap = 30 * time.Minute

// autoResetExpiryEnabled 临期槽位自动消耗开关：未配置 = 开（与 sticky_sessions
// 同款默认开语义），显式 0/false 关
func autoResetExpiryEnabled(db *DB) bool {
	v, _ := db.GetSetting("auto_reset_expiry_enabled")
	return v != "0" && v != "false"
}

var autoResetState = struct {
	sync.Mutex
	lastAttempt     map[int64]time.Time
	lastExpirySpend map[int64]time.Time
}{lastAttempt: map[int64]time.Time{}, lastExpirySpend: map[int64]time.Time{}}

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
	// auto_reset_expiry_enabled 门控临期消耗（默认开，独立于主开关）
	expiryOn := autoResetExpiryEnabled(z.db)
	autoOn := false
	if v, _ := z.db.GetSetting("auto_reset_enabled"); v == "1" {
		autoOn = true
	}
	if !autoOn && !expiryOn {
		return
	}
	// 锁保护读：a 可能正被并发额度刷新 goroutine 改写状态
	if status, _ := a.statusError(); status != StatusExhausted {
		return
	}

	autoResetState.Lock()
	if last, ok := autoResetState.lastAttempt[a.ID]; ok && time.Since(last) < autoResetAttemptInterval {
		autoResetState.Unlock()
		return
	}
	autoResetState.lastAttempt[a.ID] = time.Now()
	autoResetState.Unlock()

	// 与手动重置/领取同一把账号级锁：绝不与面板操作双消耗
	mu := z.claimLockFor(a.ID)
	if !mu.TryLock() {
		return
	}
	defer mu.Unlock()

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
	var chosen []ResetSlot
	if resetType == "FIVE_HOUR" {
		chosen = st.AvailableFiveHourResets
	} else {
		chosen = st.AvailableWeekResets
	}
	expireAt, expiring := autoResetSlotExpiringSoon(time.Now().Unix(), windowSec, chosen)
	// 阈值消耗只在主开关开时生效；主开关关时仅临期槽位可触发消耗
	if !expiring && !(autoOn && autoResetShouldSpend(waitKnown, wait, thresholdSeconds)) {
		log.Printf("[auto-reset] %s: %s window resets naturally in %dm (< threshold), keeping reset slot",
			a.DisplayNameOrEmail(), resetType, (wait+59)/60)
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

	// 配额刷新仍在单飞中：等下一轮 debounce 再评估。刷新可能恰好把账号恢复
	// active——此刻烧重置就是白烧一个稀缺槽位（fresh 重读只覆盖已落库的恢复）
	if _, busy := z.quotaRefreshInflight.Load(a.ID); busy {
		log.Printf("[auto-reset] %s: quota refresh in flight, deferring", a.DisplayNameOrEmail())
		return
	}

	used, _, msg, err := z.UseReset(a, resetType)
	// UsedAt 精确落本地执行时刻：同步去重优先精确匹配，免去 localtime
	// 墙钟换算（DST 切换时窗口会漂移出重复行）
	record := &ClaimRecord{AccountID: a.ID, Email: a.Email, TaskType: "reset", UsedAt: time.Now().Unix()}
	if err != nil || !used {
		record.Message = fmt.Sprintf("自动重置执行失败(%s): %v %s", resetType, err, msg)
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

// spendExpiringResetForSync 临期槽位消耗（非耗尽路径）：active/cooling/exhausted
// 账号的槽位没有 402 触发点，只能靠周期评估在到期前花掉。由
// SyncResetHistoryFromUpstream 在其账号级 claim 锁内调用（互斥手动/relay 路径）。
// 预筛用锁外快照 st——只为绝大多数无临期槽位的账号省掉第二次 status 请求；
// 真要动笔前必须锁内重拉 status 复核，防止快照滞后期间槽位已被 relay/手动消耗。
func (z *ZCodeAPI) spendExpiringResetForSync(a *Account, st *ResetStatus) {
	if st == nil {
		return
	}
	// 独立开关（默认开）：临期消耗不依赖 auto_reset_enabled 主开关
	if !autoResetExpiryEnabled(z.db) {
		return
	}
	windowSec := int64(settingInt(z.db, "auto_reset_expiry_spend_minutes", autoResetExpirySpendDefaultMin)) * 60
	if windowSec <= 0 {
		return
	}
	autoResetState.Lock()
	last := autoResetState.lastExpirySpend[a.ID]
	autoResetState.Unlock()
	if time.Since(last) < autoResetExpirySpendGap {
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
	// 锁内重拉复核（调用方已持 claim 锁）
	st2, _, _, err := z.FetchResetStatus(fresh)
	if err != nil {
		return
	}
	resetType := ""
	var expireAt int64
	if at, soon := autoResetSlotExpiringSoon(now, windowSec, st2.AvailableFiveHourResets); soon {
		resetType, expireAt = "FIVE_HOUR", at
	} else if at, soon := autoResetSlotExpiringSoon(now, windowSec, st2.AvailableWeekResets); soon {
		resetType, expireAt = "WEEK", at
	} else {
		return
	}
	used, _, msg, err := z.UseReset(fresh, resetType)
	record := &ClaimRecord{AccountID: fresh.ID, Email: fresh.Email, TaskType: "reset", UsedAt: time.Now().Unix()}
	if err != nil || !used {
		record.Message = fmt.Sprintf("临期自动重置执行失败(%s): %v %s", resetType, err, msg)
		z.db.InsertClaimRecord(record)
		log.Printf("[auto-reset] %s: %s", fresh.DisplayNameOrEmail(), record.Message)
		return
	}
	autoResetState.Lock()
	autoResetState.lastExpirySpend[fresh.ID] = time.Now()
	autoResetState.Unlock()
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
