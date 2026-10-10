package main

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

// R: 自动重置阈值策略——"还剩 5/20 分钟自然恢复时绝不动用重置"
func TestAutoResetShouldSpend(t *testing.T) {
	cases := []struct {
		name      string
		waitKnown bool
		waitSecs  int64
		threshold int64
		wantSpend bool
	}{
		{"unknown wait spends", false, 0, 3600, true},
		{"5min left < 60min threshold waits", true, 5 * 60, 60 * 60, false},
		{"20min left < 60min threshold waits", true, 20 * 60, 60 * 60, false},
		{"90min left >= threshold spends", true, 90 * 60, 60 * 60, true},
		{"exactly at threshold spends", true, 60 * 60, 60 * 60, true},
		{"zero threshold always spends", true, 30, 0, true},
		{"already-past window keeps (recoverable now)", true, 0, 60 * 60, false},
	}
	for _, c := range cases {
		if got := autoResetShouldSpend(c.waitKnown, c.waitSecs, c.threshold); got != c.wantSpend {
			t.Errorf("%s: autoResetShouldSpend(%v,%d,%d) = %v, want %v",
				c.name, c.waitKnown, c.waitSecs, c.threshold, got, c.wantSpend)
		}
	}
}

// 消耗决策的组合真值表（auto_reset.go 的门控表达式）：
//
//	spend = expiring || (autoOn && autoResetShouldSpend(...))
//
// 临期压过一切；阈值路径只认主开关
func TestAutoResetSpendGateComposition(t *testing.T) {
	cases := []struct {
		name      string
		autoOn    bool
		expiring  bool
		waitKnown bool
		waitSecs  int64
		threshold int64
		wantSpend bool
	}{
		{"expiry window hit spends even with master off", false, true, true, 5 * 60, 60 * 60, true},
		{"master off + not expiring keeps", false, false, true, 5 * 60, 60 * 60, false},
		{"master on + threshold pass spends", true, false, true, 90 * 60, 60 * 60, true},
		{"master on + threshold fail keeps", true, false, true, 5 * 60, 60 * 60, false},
		{"master on + threshold fail but expiring spends", true, true, true, 5 * 60, 60 * 60, true},
		{"unknown wait spends under master", true, false, false, 0, 60 * 60, true},
	}
	for _, c := range cases {
		got := c.expiring || (c.autoOn && autoResetShouldSpend(c.waitKnown, c.waitSecs, c.threshold))
		if got != c.wantSpend {
			t.Errorf("%s: gate(autoOn=%v, expiring=%v) = %v, want %v",
				c.name, c.autoOn, c.expiring, got, c.wantSpend)
		}
	}
}

func TestAutoResetWait(t *testing.T) {
	// 无快照 / 无 next_reset → 未知
	if _, ok := autoResetWait(&Account{}); ok {
		t.Error("empty quota snapshot must report unknown wait")
	}
	if _, ok := autoResetWait(&Account{QuotaJSON: `{"next_reset":0}`}); ok {
		t.Error("zero next_reset must report unknown wait")
	}
	// 未来 30 分钟 → 已知 1800s（±2s 容差）
	future := time.Now().Add(30 * time.Minute).Unix()
	wait, ok := autoResetWait(&Account{QuotaJSON: `{"next_reset":` + strconv.FormatInt(future, 10) + `}`})
	if !ok || wait < 1798 || wait > 1800 {
		t.Errorf("future reset: wait=%d ok=%v, want ~1800/true", wait, ok)
	}
	// 过去的 next_reset → 0（已可自然恢复）
	past := time.Now().Add(-time.Minute).Unix()
	wait, ok = autoResetWait(&Account{QuotaJSON: `{"next_reset":` + strconv.FormatInt(past, 10) + `}`})
	if !ok || wait != 0 {
		t.Errorf("past reset: wait=%d ok=%v, want 0/true", wait, ok)
	}
}

// R: 临期槽位 use-it-lose-it——槽位进入消耗窗口才触发；已过期/无到期时间/窗口外
// 一律不触发（花已过期槽位必然失败，无到期时间视为无限期按原阈值策略走）
func TestAutoResetSlotExpiringSoon(t *testing.T) {
	const now = int64(1_000_000)
	const hour = int64(3600)
	slots := []ResetSlot{{ExpireAt: now + 45*60}, {ExpireAt: now + 20*60}}

	at, ok := autoResetSlotExpiringSoon(now, 60*60, slots)
	if !ok || at != now+20*60 {
		t.Errorf("within window: got (%d,%v), want earliest expiring (%d,true)", at, ok, now+20*60)
	}
	if _, ok := autoResetSlotExpiringSoon(now, 10*60, slots); ok {
		t.Error("slot beyond window must not trigger")
	}
	if _, ok := autoResetSlotExpiringSoon(now, 0, slots); ok {
		t.Error("zero window (disabled) must not trigger")
	}
	if _, ok := autoResetSlotExpiringSoon(now, 60*60, nil); ok {
		t.Error("empty slots must not trigger")
	}
	if _, ok := autoResetSlotExpiringSoon(now, 60*60, []ResetSlot{{ExpireAt: now - 60}}); ok {
		t.Error("already-expired slot must not trigger")
	}
	if _, ok := autoResetSlotExpiringSoon(now, 60*60, []ResetSlot{{ExpireAt: 0}}); ok {
		t.Error("unknown expiry (0) must not trigger")
	}
	// 恰好压窗口边界（expire_at == now+window）属于窗口内
	if _, ok := autoResetSlotExpiringSoon(now, 60*60, []ResetSlot{{ExpireAt: now + 60*60}}); !ok {
		t.Error("slot exactly at window edge must trigger")
	}
}

// R: 防双花闸门为两条消耗路径共享（MaybeAutoReset 与 spendExpiringResetForSync）：
// A 路径消耗成功即盖印；gap 内同到期槽位的 B 路径必须被拒且不动笔（上游 status
// 滞后的双花形态）；不同到期槽位或 gap 过后放行；动笔失败不盖印（下轮可重试）
func TestExpirySpendGuardedGap(t *testing.T) {
	const id = int64(990102)
	clean := func() {
		autoResetState.Lock()
		delete(autoResetState.lastExpirySpend, id)
		delete(autoResetState.lastExpirySpendSlot, id)
		autoResetState.Unlock()
	}
	clean()
	defer clean()

	calls := 0
	okSpend := func() error { calls++; return nil }

	// 路径 A：消耗成功并盖印（时刻 + 槽位到期时间）
	spent, reason := spendExpiringSlotGuarded(id, 1000, okSpend)
	if !spent || reason != "" || calls != 1 {
		t.Fatalf("first spend: spent=%v reason=%q calls=%d, want true/\"\"/1", spent, reason, calls)
	}
	autoResetState.Lock()
	stamp, slot := autoResetState.lastExpirySpend[id], autoResetState.lastExpirySpendSlot[id]
	autoResetState.Unlock()
	if time.Since(stamp) > time.Minute || slot != 1000 {
		t.Fatalf("stamp after spend: stamp=%v slot=%d, want recent/1000", stamp, slot)
	}

	// 路径 B：gap 内同到期槽位——拒绝且不动笔
	spent, reason = spendExpiringSlotGuarded(id, 1000, okSpend)
	if spent || reason != spendGuardGapRefused || calls != 1 {
		t.Fatalf("gap same-slot: spent=%v reason=%q calls=%d, want false/%s/1", spent, reason, calls, spendGuardGapRefused)
	}

	// gap 内不同到期槽位：放行（锁内重拉列表即上游亲证是另一槽）
	spent, _ = spendExpiringSlotGuarded(id, 2000, okSpend)
	if !spent || calls != 2 {
		t.Fatalf("gap different-slot: spent=%v calls=%d, want true/2", spent, calls)
	}

	// gap 过后同槽位：放行
	autoResetState.Lock()
	autoResetState.lastExpirySpend[id] = time.Now().Add(-autoResetExpirySpendGap - time.Minute)
	autoResetState.Unlock()
	spent, _ = spendExpiringSlotGuarded(id, 1000, okSpend)
	if !spent || calls != 3 {
		t.Fatalf("post-gap same-slot: spent=%v calls=%d, want true/3", spent, calls)
	}

	// 动笔失败：不盖印（下轮必须还能重试）
	autoResetState.Lock()
	stampBefore, slotBefore := autoResetState.lastExpirySpend[id], autoResetState.lastExpirySpendSlot[id]
	autoResetState.Unlock()
	spent, reason = spendExpiringSlotGuarded(id, 3000, func() error { calls++; return errors.New("boom") })
	if spent || reason != "boom" || calls != 4 {
		t.Fatalf("failed spend: spent=%v reason=%q calls=%d, want false/boom/4", spent, reason, calls)
	}
	autoResetState.Lock()
	stampAfter, slotAfter := autoResetState.lastExpirySpend[id], autoResetState.lastExpirySpendSlot[id]
	autoResetState.Unlock()
	if !stampAfter.Equal(stampBefore) || slotAfter != slotBefore {
		t.Fatalf("failed spend must not stamp: stamp %v→%v slot %d→%d, want unchanged", stampBefore, stampAfter, slotBefore, slotAfter)
	}
}

// R: 防双花印本是内存态、进程重启清零——重启落在上游 status 滞后窗口内时，
// 同槽位可能被再花一次。内存无印时必须以账本兜底：30 分钟内有成功重置记录
// 即按 slot=0 无归属语义保守拒绝；记录老化出 gap 后恢复放行。
// 闸门本身只认内存印（冻结契约），拦截责任在调用点的 expirySpendColdBlocked
func TestSpendGuardDBBootstrap(t *testing.T) {
	db, _ := newVaultTestDB(t)
	z := &ZCodeAPI{db: db, claimLocks: map[int64]*sync.Mutex{}}
	const id = int64(990105)
	clean := func() {
		autoResetState.Lock()
		delete(autoResetState.lastExpirySpend, id)
		delete(autoResetState.lastExpirySpendSlot, id)
		autoResetState.Unlock()
	}
	clean()
	defer clean()

	calls := 0
	spend := func() error { calls++; return nil }
	if err := db.InsertClaimRecord(&ClaimRecord{
		AccountID: id, Email: "bootstrap@test", TaskType: "reset",
		PlanName: "配额重置(FIVE_HOUR)[自动]", Success: true, UsedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("insert recent reset record: %v", err)
	}

	// 内存无印 + 账本 30 分钟内有成功重置：冷启动兜底拦截（调用点据此不动笔）
	if !z.expirySpendColdBlocked(id) {
		t.Fatal("cold start with recent ledger spend must be blocked")
	}

	// 记录老化出 gap：放行（账本只兜底 gap 窗口，不永久封锁）
	if _, err := db.conn.Exec(`UPDATE claim_records SET used_at=? WHERE account_id=?`,
		time.Now().Add(-autoResetExpirySpendGap-time.Minute).Unix(), id); err != nil {
		t.Fatalf("age record: %v", err)
	}
	if z.expirySpendColdBlocked(id) {
		t.Fatal("aged ledger record must not block")
	}
	if spent, _ := spendExpiringSlotGuarded(id, 1000, spend); !spent || calls != 1 {
		t.Fatalf("aged ledger record must allow spend: spent=%v calls=%d, want true/1", spent, calls)
	}

	// 热路径：内存有印时零开销短路，不查库（无归属印记照旧由闸门拒绝）
	autoResetState.Lock()
	autoResetState.lastExpirySpend[id] = time.Now()
	autoResetState.lastExpirySpendSlot[id] = 0
	autoResetState.Unlock()
	if z.expirySpendColdBlocked(id) {
		t.Fatal("warm memory stamp must skip ledger consult")
	}
	if _, reason := spendExpiringSlotGuarded(id, 9999, spend); reason != spendGuardGapRefused || calls != 1 {
		t.Fatalf("warm slot=0 stamp: reason=%q calls=%d, want %s/1", reason, calls, spendGuardGapRefused)
	}
}

// R: 幂等键只在临期路径确定性化（G1 回归锁）——阈值路径的 expireAt 恒 0，
// 若也走确定性键会退化为 "auto-expiry-<id>-0" 常量：同一账号两次合法阈值
// 消耗（本次 five_hour、下周 week）撞成同键，上游幂等真生效时第二次被误拒
func TestAutoResetIdemKeyThresholdEmpty(t *testing.T) {
	if got := autoResetIdemKey(false, 7, 0); got != "" {
		t.Errorf("threshold path: got %q, want empty (random UUID fallback)", got)
	}
	if got := autoResetIdemKey(true, 7, 1000); got != "auto-expiry-7-1000" {
		t.Errorf("expiring path: got %q, want auto-expiry-7-1000", got)
	}
}

// R: 阈值路径防重放闸（G2 回归锁）——任一路径 30 分钟内成功消耗过（暖印或
// 冷启动账本）即暂缓；印老化出 gap 且账本干净后恢复放行
func TestThresholdSpendDeferred(t *testing.T) {
	db, _ := newVaultTestDB(t)
	z := &ZCodeAPI{db: db, claimLocks: map[int64]*sync.Mutex{}}
	const id = int64(990107)
	clean := func() {
		autoResetState.Lock()
		delete(autoResetState.lastExpirySpend, id)
		delete(autoResetState.lastExpirySpendSlot, id)
		autoResetState.Unlock()
	}
	clean()
	defer clean()

	// 干净状态：不暂缓
	if z.thresholdSpendDeferred(id) {
		t.Fatal("clean state must not defer threshold spend")
	}
	// 暖印在 gap 内：暂缓
	autoResetState.Lock()
	autoResetState.lastExpirySpend[id] = time.Now()
	autoResetState.lastExpirySpendSlot[id] = 0
	autoResetState.Unlock()
	if !z.thresholdSpendDeferred(id) {
		t.Fatal("warm stamp within gap must defer threshold spend")
	}
	// 印老化出 gap、账本无记录：放行
	autoResetState.Lock()
	autoResetState.lastExpirySpend[id] = time.Now().Add(-autoResetExpirySpendGap - time.Minute)
	autoResetState.Unlock()
	if z.thresholdSpendDeferred(id) {
		t.Fatal("aged warm stamp with clean ledger must not defer")
	}
	// 冷启动（内存无印）+ 账本 gap 内有成功重置：暂缓
	autoResetState.Lock()
	delete(autoResetState.lastExpirySpend, id)
	autoResetState.Unlock()
	if err := db.InsertClaimRecord(&ClaimRecord{
		AccountID: id, Email: "thresh@test", TaskType: "reset",
		PlanName: "配额重置(WEEK)", Success: true, UsedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("insert record: %v", err)
	}
	if !z.thresholdSpendDeferred(id) {
		t.Fatal("cold start with recent ledger spend must defer threshold spend")
	}
}

// R: 临期消耗开关默认关（2026-10-09 收紧）——未配置/0/false 一律关，显式 "1" 才开。
// "自动动用重置机会"整体 opt-in，默认态下任何路径都不得动槽位
func TestAutoResetExpiryDefaultOff(t *testing.T) {
	db, _ := newVaultTestDB(t)
	if autoResetExpiryEnabled(db) {
		t.Error("unset auto_reset_expiry_enabled must default to OFF")
	}
	for _, v := range []string{"0", "false", "garbage"} {
		db.SetSetting("auto_reset_expiry_enabled", v)
		if autoResetExpiryEnabled(db) {
			t.Errorf("auto_reset_expiry_enabled=%q must be OFF", v)
		}
	}
	db.SetSetting("auto_reset_expiry_enabled", "1")
	if !autoResetExpiryEnabled(db) {
		t.Error("auto_reset_expiry_enabled=1 must be ON")
	}
}

// R: expire_at 量纲归一——上游毫秒已实证（ZCode 客户端 app.asar 的 vpt() 把
// expireAt 与 Date.now() 毫秒直接比较；同接口 used_at 亦毫秒）。与 epoch 秒
// 比较的临期判定必须归一，否则永不命中（use-it-or-lose-it 静默失效）。
// >1e11 只可能是毫秒：归一到秒；秒级值与 0 一律原样保留
func TestNormalizeResetSlotUnits(t *testing.T) {
	ms := int64(1791478335000)
	sec := int64(1791478335)
	st := &ResetStatus{
		AvailableFiveHourResets: []ResetSlot{{ExpireAt: ms}, {ExpireAt: sec}, {ExpireAt: 0}},
		AvailableWeekResets:     []ResetSlot{{ExpireAt: ms + 60000}},
	}
	normalizeResetSlotUnits(st)
	if st.AvailableFiveHourResets[0].ExpireAt != sec {
		t.Errorf("ms value: got %d, want normalized %d", st.AvailableFiveHourResets[0].ExpireAt, sec)
	}
	if st.AvailableFiveHourResets[1].ExpireAt != sec {
		t.Errorf("seconds value must pass through: got %d", st.AvailableFiveHourResets[1].ExpireAt)
	}
	if st.AvailableFiveHourResets[2].ExpireAt != 0 {
		t.Errorf("zero (unknown expiry) must pass through: got %d", st.AvailableFiveHourResets[2].ExpireAt)
	}
	if st.AvailableWeekResets[0].ExpireAt != ms/1000+60 {
		t.Errorf("week ms value: got %d, want %d", st.AvailableWeekResets[0].ExpireAt, ms/1000+60)
	}
	normalizeResetSlotUnits(nil) // 不得 panic
}

// R: 无 JWT 的账号（纯 API Key）永远走不了 JWT 通道重置——自动评估必须提前
// 静默让路，否则每轮记一条"需要 ZCode JWT"失败日志且永无成功可能
func TestMaybeAutoResetJWTLessAccountNoStamp(t *testing.T) {
	db, _ := newVaultTestDB(t)
	z := &ZCodeAPI{db: db, claimLocks: map[int64]*sync.Mutex{}}
	db.SetSetting("auto_reset_expiry_enabled", "1") // 开关开着也必须被 JWT 门拦下
	a := &Account{ID: 990106, Email: "nojwt@test", Status: StatusExhausted}
	z.MaybeAutoReset(a, "test")
	autoResetState.Lock()
	_, stamped := autoResetState.lastAttempt[a.ID]
	autoResetState.Unlock()
	if stamped {
		t.Error("JWT-less account must return before debouncing/stamping")
	}
}

// R: 防抖印记不得在拿到 claim 锁之前落——锁忙早退不是一次真实评估，先盖印
// 会让一次锁竞争吃掉整个 10 分钟评估窗口（耗尽账号不再有下一触发）
func TestMaybeAutoResetLockBusyLeavesStaleStamp(t *testing.T) {
	db, _ := newVaultTestDB(t)
	z := &ZCodeAPI{db: db, claimLocks: map[int64]*sync.Mutex{}}
	// 临期开关显式开：默认态下函数在开关门就返回，到不了 TryLock，测试变空转
	db.SetSetting("auto_reset_expiry_enabled", "1")
	a := &Account{ID: 990103, Email: "busy-lock@test", Status: StatusExhausted, ZCodeJWT: "test-jwt"}
	mu := z.claimLockFor(a.ID)
	mu.Lock() // 模拟领取/手动流程并发持有 claim 锁
	defer mu.Unlock()
	autoResetState.Lock()
	autoResetState.lastAttempt[a.ID] = time.Now().Add(-2 * autoResetAttemptInterval) // 陈旧印记
	autoResetState.Unlock()
	defer func() {
		autoResetState.Lock()
		delete(autoResetState.lastAttempt, a.ID)
		autoResetState.Unlock()
	}()

	z.MaybeAutoReset(a, "test")

	autoResetState.Lock()
	st := autoResetState.lastAttempt[a.ID]
	autoResetState.Unlock()
	if time.Since(st) < autoResetAttemptInterval {
		t.Fatalf("lock-busy early return refreshed lastAttempt: age=%v, want stale (>= %v)", time.Since(st), autoResetAttemptInterval)
	}
}
