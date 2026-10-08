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

const autoResetAttemptInterval = 10 * time.Minute

var autoResetState = struct {
	sync.Mutex
	lastAttempt map[int64]time.Time
}{lastAttempt: map[int64]time.Time{}}

// autoResetShouldSpend 阈值判定（纯函数，供测试）：等待未知 = 花；
// 等待已知且 >= 阈值 = 花；否则等自然恢复
func autoResetShouldSpend(waitKnown bool, waitSeconds int64, thresholdSeconds int64) bool {
	if !waitKnown {
		return true
	}
	return waitSeconds >= thresholdSeconds
}

// autoResetWait 从账号额度快照取自然窗口重置等待（秒）；未知返回 ok=false
func autoResetWait(a *Account) (int64, bool) {
	if a.QuotaJSON == "" {
		return 0, false
	}
	var ov struct {
		NextReset int64 `json:"next_reset"`
	}
	if json.Unmarshal([]byte(a.QuotaJSON), &ov) != nil || ov.NextReset <= 0 {
		return 0, false
	}
	wait := ov.NextReset - time.Now().Unix()
	if wait < 0 {
		wait = 0
	}
	return wait, true
}

// MaybeAutoReset 耗尽后的自动重置评估（非阻塞调用：内部自持锁，失败只记日志）。
// trigger 仅用于日志定位（relay / quota / manual-check）。
func (z *ZCodeAPI) MaybeAutoReset(a *Account, trigger string) {
	if v, _ := z.db.GetSetting("auto_reset_enabled"); v != "1" {
		return
	}
	if a.Status != StatusExhausted {
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
	if !autoResetShouldSpend(waitKnown, wait, thresholdSeconds) {
		log.Printf("[auto-reset] %s: %s window resets naturally in %dm (< threshold), keeping reset slot",
			a.DisplayNameOrEmail(), resetType, (wait+59)/60)
		return
	}

	used, _, msg, err := z.UseReset(a, resetType)
	record := &ClaimRecord{AccountID: a.ID, Email: a.Email, TaskType: "reset"}
	if err != nil || !used {
		record.Message = fmt.Sprintf("自动重置执行失败(%s): %v %s", resetType, err, msg)
		z.db.InsertClaimRecord(record)
		z.db.SetAccountClaimResult(a.ID, "配额重置", record.Message)
		log.Printf("[auto-reset] %s: %s", a.DisplayNameOrEmail(), record.Message)
		return
	}
	record.Success = true
	record.PlanName = "配额重置(" + resetType + ")[自动]"
	record.Message = "自动重置成功（耗尽触发，等待 " + humanizeWait(wait, waitKnown) + " 超过阈值），配额已恢复"
	z.db.InsertClaimRecord(record)
	z.db.SetAccountClaimResult(a.ID, record.PlanName, record.Message)
	log.Printf("[auto-reset] %s: quota restored via %s (trigger=%s)", a.DisplayNameOrEmail(), resetType, trigger)
	go func() {
		time.Sleep(2 * time.Second)
		z.RefreshAccountQuota(a)
	}()
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
