package main

import (
	"path/filepath"
	"testing"
	"time"
)

func newPoolTestPool(t *testing.T) (*AccountPool, *DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pool-test.db")
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewAccountPool(db, &FileConfig{}, "3.14.4"), db
}

func mkPoolAccount(id string, pri int64, status string) *Account {
	return &Account{
		UserID: id, Email: id + "@test", Provider: "zai", AuthType: "jwt",
		ZCodeJWT: "jwt-" + id, Status: status, Enabled: true, Priority: pri,
	}
}

// F1：priority 策略级联——促销层（数值小）先用，耗尽让位，恢复回归
func TestPriorityStrategyCascade(t *testing.T) {
	p, db := newPoolTestPool(t)
	if err := db.SetSetting("selection_strategy", StrategyPriority); err != nil {
		t.Fatal(err)
	}
	promo, main := mkPoolAccount("promo", PromoPriority, StatusActive), mkPoolAccount("main", DefaultPriority, StatusActive)
	promoID, err := db.UpsertAccount(promo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertAccount(main); err != nil {
		t.Fatal(err)
	}
	if got := p.Select("zai", "", nil); got == nil || got.UserID != "promo" {
		t.Fatalf("expected promo tier first, got %+v", got)
	}
	if err := db.SetAccountStatus(promoID, StatusExhausted, "drained", 0); err != nil {
		t.Fatal(err)
	}
	if got := p.Select("zai", "", nil); got == nil || got.UserID != "main" {
		t.Fatalf("expected cascade to main after promo exhausted, got %+v", got)
	}
	if err := db.SetAccountStatus(promoID, StatusActive, "", 0); err != nil {
		t.Fatal(err)
	}
	if got := p.Select("zai", "", nil); got == nil || got.UserID != "promo" {
		t.Fatalf("expected promo tier on recovery, got %+v", got)
	}
}

// F2：会话粘滞——同会话固定账号，粘滞账号不可用时让位
func TestStickySessions(t *testing.T) {
	p, db := newPoolTestPool(t)
	if err := db.SetSetting("selection_strategy", StrategyRoundRobin); err != nil {
		t.Fatal(err)
	}
	a1, a2 := mkPoolAccount("s1", DefaultPriority, StatusActive), mkPoolAccount("s2", DefaultPriority, StatusActive)
	if _, err := db.UpsertAccount(a1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertAccount(a2); err != nil {
		t.Fatal(err)
	}
	first := p.SelectSticky("zai", "", "sess-x", nil)
	if first == nil {
		t.Fatal("nil selection")
	}
	for i := 0; i < 5; i++ {
		again := p.SelectSticky("zai", "", "sess-x", nil)
		if again == nil || again.ID != first.ID {
			t.Fatalf("sticky broken at round %d: %v vs %d", i, again, first.ID)
		}
	}
	// 粘滞账号停用 → 自动让位（且新粘滞落到另一账号）
	if err := db.UpdateAccountFields(first.ID, "", "", false); err != nil {
		t.Fatal(err)
	}
	other := p.SelectSticky("zai", "", "sess-x", nil)
	if other == nil || other.ID == first.ID {
		t.Fatalf("expected fallback after disable, got %+v", other)
	}
	// skip 中的账号不参与粘滞：重新启用 s1，跳过 s2 → 应选中 s1
	if err := db.UpdateAccountFields(first.ID, "", "", true); err != nil {
		t.Fatal(err)
	}
	third := p.SelectSticky("zai", "", "sess-y", map[int64]bool{other.ID: true})
	if third == nil || third.ID == other.ID {
		t.Fatalf("skip not honored: %+v", third)
	}
}

// F3/F5 辅助：重置时间解析 + 促销档识别
func TestIsPromoTierAndNextReset(t *testing.T) {
	for _, s := range []string{"Start Plan", "体验版", "TRIAL", "promo-weekend"} {
		if !isPromoTier(s) {
			t.Fatalf("%q should be promo tier", s)
		}
	}
	for _, s := range []string{"Max", "Pro", "Lite", ""} {
		if isPromoTier(s) {
			t.Fatalf("%q should not be promo tier", s)
		}
	}
	if got := nextResetFromQuota(`{"next_reset":1790708414}`); got != 1790708414 {
		t.Fatalf("next_reset parse: %d", got)
	}
	if got := nextResetFromQuota(`{}`); got != 0 {
		t.Fatalf("missing next_reset should be 0, got %d", got)
	}
}

// F4：双通道概要提取
func TestChannelSummary(t *testing.T) {
	rem := 123.45
	ov := &QuotaOverview{Source: "api.z.ai/monitor", PlanTier: "Max", Remaining: &rem, NextReset: 1790708414}
	c := channelSummary(ov)
	if c.Source != "api.z.ai/monitor" || c.PlanTier != "Max" || c.Remaining != 123.45 || c.NextReset != 1790708414 || c.Exhausted {
		t.Fatalf("bad channel summary: %+v", c)
	}
}

// 并发闸门：上限排队、释放恢复、1302 限流体识别、冷却升级
func TestAccountSlotGate(t *testing.T) {
	p, db := newPoolTestPool(t)
	a := mkPoolAccount("slot", DefaultPriority, StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	a.ID = id
	db.SetSetting("max_concurrent_per_account", "2")

	if !p.AcquireAccountSlot(a, 50*time.Millisecond) {
		t.Fatal("first acquire should succeed")
	}
	if !p.AcquireAccountSlot(a, 50*time.Millisecond) {
		t.Fatal("second acquire should succeed")
	}
	if p.AcquireAccountSlot(a, 50*time.Millisecond) {
		t.Fatal("third acquire over cap should time out")
	}
	p.ReleaseAccountSlot(a)
	if !p.AcquireAccountSlot(a, 50*time.Millisecond) {
		t.Fatal("acquire after release should succeed")
	}
	p.ReleaseAccountSlot(a)
	p.ReleaseAccountSlot(a) // 幂等：多余释放不 panic 不负计数
	if !p.AcquireAccountSlot(a, 50*time.Millisecond) {
		t.Fatal("idempotent double-release should not corrupt the gate")
	}
	p.ReleaseAccountSlot(a)
}

func TestIsRateLimitBody(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, `anything`, true},
		{400, `{"code":1302,"msg":"Rate limit reached for requests"}`, true},
		{400, `{"code": 1303}`, true},
		{200, `[1302][Rate limit reached for requests][20260930…]`, true},
		{400, `{"code":3012,"msg":"unusual activity"}`, false},
		{400, `{"code":1003,"msg":"already claimed"}`, false},
	}
	for _, c := range cases {
		if got := isRateLimitBody(c.status, c.body); got != c.want {
			t.Fatalf("isRateLimitBody(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestNextRateLimitCooldownEscalates(t *testing.T) {
	a := mkPoolAccount("esc", DefaultPriority, StatusActive)
	if got := nextRateLimitCooldown(a); got != 30 {
		t.Fatalf("first offense should be 30s, got %d", got)
	}
	a.LastError = "上游限流（HTTP 429，model=x），冷却 30s"
	if got := nextRateLimitCooldown(a); got != 120 {
		t.Fatalf("second offense should be 120s, got %d", got)
	}
	a.LastError = "上游限流（HTTP 429，model=x），冷却 120s"
	if got := nextRateLimitCooldown(a); got != 300 {
		t.Fatalf("third offense should be 300s, got %d", got)
	}
	a.LastError = "上游限流（HTTP 429，model=x），冷却 300s"
	if got := nextRateLimitCooldown(a); got != 300 {
		t.Fatalf("should cap at 300s, got %d", got)
	}
}
