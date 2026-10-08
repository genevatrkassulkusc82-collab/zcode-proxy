package main

import (
	"path/filepath"
	"testing"
	"time"
)

// ---- 免费优先 / 付费回退：通道级可选性、闸门隔离、冷却升级、日上限 ----

func newPaidTestPool(t *testing.T) (*AccountPool, *DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paid-fallback-test.db")
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewAccountPool(db, &FileConfig{}, "3.14.4"), db
}

// mkDualAccount 双通道账号（JWT 免费额度 + APIKey 按量计费）
func mkDualAccount(id, status string) *Account {
	return &Account{
		UserID: id, Email: id + "@test", Provider: "zai", AuthType: "jwt",
		ZCodeJWT: "jwt-" + id, APIKey: "key-" + id,
		Status: status, Enabled: true,
	}
}

// 免费耗尽的账号：免费阶段不可选，付费回退阶段仍可选（付费通道独立状态）
func TestExhaustedFreeAccountStillPaidSelectable(t *testing.T) {
	p, db := newPaidTestPool(t)
	a := mkDualAccount("drained", StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountStatus(id, StatusExhausted, "免费额度用完", 0); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()

	fresh, err := db.GetAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	if selectableForFreePhase(fresh, now) {
		t.Fatal("free phase must skip exhausted account")
	}
	if !paidChannelAvailable(fresh, now) {
		t.Fatal("paid fallback must still accept exhausted-free account")
	}
	if got := p.SelectChannel("zai", "", nil, ChannelFree); got != nil {
		t.Fatalf("free phase selected exhausted account: %+v", got)
	}
	if got := p.SelectChannel("zai", "", nil, ChannelPaid); got == nil || got.UserID != "drained" {
		t.Fatalf("paid phase should select exhausted-free account, got %+v", got)
	}
}

// 每账号付费回退开关：关掉后付费通道不可选（免费侧不受影响）
func TestPaidFallbackToggle(t *testing.T) {
	p, db := newPaidTestPool(t)
	a := mkDualAccount("toggled", StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	// 关闭付费回退（group/remark/enabled 保持原值）
	if err := db.UpdateAccountFields(id, a.AccountGroup, a.Remark, true, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	fresh, err := db.GetAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	if paidChannelAvailable(fresh, now) {
		t.Fatal("paid channel must be unavailable after toggle-off")
	}
	if !selectableForFreePhase(fresh, now) {
		t.Fatal("free channel must remain selectable after paid toggle-off")
	}
	if got := p.SelectChannel("zai", "", nil, ChannelPaid); got != nil {
		t.Fatalf("paid phase should skip toggled-off account, got %+v", got)
	}
	// 重新开启
	if err := db.UpdateAccountFields(id, a.AccountGroup, a.Remark, true, true); err != nil {
		t.Fatal(err)
	}
	if got := p.SelectChannel("zai", "", nil, ChannelPaid); got == nil {
		t.Fatal("paid phase should select after toggle-on")
	}
}

// 付费通道冷却：限流冷却与余额不足长冷却都只挡付费通道
func TestPaidCoolingIndependentOfFree(t *testing.T) {
	p, db := newPaidTestPool(t)
	a := mkDualAccount("cooled", StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	a.ID = id
	p.MarkPaidCooling(a, "付费通道限流", 120)
	now := time.Now().Unix()

	fresh, err := db.GetAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != StatusActive {
		t.Fatalf("paid cooling must not touch free status, got %s", fresh.Status)
	}
	if paidChannelAvailable(fresh, now) {
		t.Fatal("paid channel must be blocked while cooling")
	}
	if !selectableForFreePhase(fresh, now) {
		t.Fatal("free channel must remain selectable during paid cooling")
	}
	// 付费成功清冷却，且不动免费侧状态
	p.MarkPaidUsed(a)
	fresh2, err := db.GetAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	if fresh2.PaidCoolingUntil != 0 {
		t.Fatalf("paid success should clear paid cooling, got %d", fresh2.PaidCoolingUntil)
	}
	if fresh2.Status != StatusActive {
		t.Fatalf("paid success must not alter free status, got %s", fresh2.Status)
	}
}

// 免费侧冷却/耗尽不牵连付费通道；invalid 账号的 API Key 仍可回退
func TestFreeSideStateDoesNotBlockPaid(t *testing.T) {
	p, db := newPaidTestPool(t)
	for _, status := range []string{StatusCooling, StatusExhausted, StatusInvalid, StatusInactive} {
		a := mkDualAccount("free-"+status, status)
		id, err := db.UpsertAccount(a)
		if err != nil {
			t.Fatal(err)
		}
		if status == StatusCooling {
			// cooling_until 设到未来，模拟限流中
			if err := db.SetAccountStatus(id, StatusCooling, "限流", time.Now().Add(10*time.Minute).Unix()); err != nil {
				t.Fatal(err)
			}
		}
		if got := p.SelectChannel("zai", "", nil, ChannelPaid); got == nil || got.UserID != "free-"+status {
			t.Fatalf("paid fallback should accept %s account, got %+v", status, got)
		}
		if err := db.DeleteAccount(id); err != nil {
			t.Fatal(err)
		}
	}
}

// 付费限流冷却升级 30→120→300，成功使用后清零
func TestPaidCooldownEscalation(t *testing.T) {
	p, db := newPaidTestPool(t)
	a := mkDualAccount("escalate", StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	a.ID = id
	for _, want := range []int{30, 120, 300, 300} {
		if got := p.nextPaidCooldown(a); got != want {
			t.Fatalf("nextPaidCooldown = %d, want %d", got, want)
		}
		p.MarkPaidCooling(a, "限流", want)
	}
	p.MarkPaidUsed(a)
	if got := p.nextPaidCooldown(a); got != 30 {
		t.Fatalf("cooldown memory should reset on success, got %d", got)
	}
}

// 免费/付费并发闸门互相隔离：免费侧打满不堵付费回退
func TestPerChannelSlotGates(t *testing.T) {
	p, db := newPaidTestPool(t)
	a := mkDualAccount("gated", StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	a.ID = id
	db.SetSetting("max_concurrent_per_account", "1")

	relFree, ok := p.AcquireAccountSlot(a, ChannelFree, 50*time.Millisecond)
	if !ok {
		t.Fatal("free slot acquire should succeed")
	}
	if _, ok := p.AcquireAccountSlot(a, ChannelFree, 50*time.Millisecond); ok {
		t.Fatal("free slot over cap should time out")
	}
	relPaid, ok := p.AcquireAccountSlot(a, ChannelPaid, 50*time.Millisecond)
	if !ok {
		t.Fatal("paid slot must be independent of full free slot")
	}
	relPaid()
	relFree()
}

// 付费回退策略读取：默认 free_first，非法值回落
func TestPaidFallbackPolicySetting(t *testing.T) {
	p, db := newPaidTestPool(t)
	if got := p.PaidFallbackPolicy(); got != PaidModeFreeFirst {
		t.Fatalf("default policy = %s, want %s", got, PaidModeFreeFirst)
	}
	for _, mode := range []string{PaidModeNever, PaidModeBalanced, PaidModeFreeFirst} {
		if err := db.SetSetting("paid_fallback_mode", mode); err != nil {
			t.Fatal(err)
		}
		if got := p.PaidFallbackPolicy(); got != mode {
			t.Fatalf("policy = %s, want %s", got, mode)
		}
	}
	if err := db.SetSetting("paid_fallback_mode", "bogus"); err != nil {
		t.Fatal(err)
	}
	if got := p.PaidFallbackPolicy(); got != PaidModeFreeFirst {
		t.Fatalf("bogus policy should fall back to %s, got %s", PaidModeFreeFirst, got)
	}
}

// 付费日上限统计：只累计 paid 通道记录（旧记录空 channel 按 free 不计）
func TestPaidTokensToday(t *testing.T) {
	_, db := newPaidTestPool(t)
	recs := []*UsageRecord{
		{AccountID: 1, TotalTokens: 100, Channel: "paid"},
		{AccountID: 1, TotalTokens: 40, Channel: "paid"},
		{AccountID: 2, TotalTokens: 500, Channel: "free"},
		{AccountID: 2, TotalTokens: 70}, // 旧记录：无 channel
	}
	for _, r := range recs {
		if err := db.InsertUsageRecord(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.PaidTokensToday()
	if err != nil {
		t.Fatal(err)
	}
	if got != 140 {
		t.Fatalf("PaidTokensToday = %d, want 140", got)
	}
}

// paid_daily_token_cap 设置解析：缺省/非法 = 0（不限）
func TestPaidDailyTokenCapSetting(t *testing.T) {
	p, db := newPaidTestPool(t)
	if got := p.paidDailyTokenCap(); got != 0 {
		t.Fatalf("default cap = %d, want 0", got)
	}
	if err := db.SetSetting("paid_daily_token_cap", "50000"); err != nil {
		t.Fatal(err)
	}
	if got := p.paidDailyTokenCap(); got != 50000 {
		t.Fatalf("cap = %d, want 50000", got)
	}
	if err := db.SetSetting("paid_daily_token_cap", "-3"); err != nil {
		t.Fatal(err)
	}
	if got := p.paidDailyTokenCap(); got != 0 {
		t.Fatalf("negative cap should normalize to 0, got %d", got)
	}
}

// 纯 API Key 账号（无 JWT）：paid_only 伪通道只匹配它们（never 策略兜底）；
// 耗尽（唯一通道额度完）后不参与任何阶段
func TestAPIKeyOnlyAccountSelection(t *testing.T) {
	p, db := newPaidTestPool(t)
	a := &Account{
		UserID: "keyonly", Email: "keyonly@test", Provider: "zai", AuthType: "apikey",
		APIKey: "key-only", Status: StatusActive, Enabled: true,
	}
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}

	if got := p.SelectChannel("zai", "", nil, ChannelPaidOnly); got == nil || got.UserID != "keyonly" {
		t.Fatalf("paid_only should select apikey-only account, got %+v", got)
	}
	if got := p.SelectChannel("zai", "", nil, ChannelFree); got != nil {
		t.Fatalf("free phase must skip apikey-only account, got %+v", got)
	}
	if got := p.SelectChannel("zai", "", nil, ChannelPaid); got == nil || got.UserID != "keyonly" {
		t.Fatalf("paid phase should select apikey-only account, got %+v", got)
	}

	dual := mkDualAccount("dual", StatusActive)
	if _, err := db.UpsertAccount(dual); err != nil {
		t.Fatal(err)
	}
	// 双通道账号不匹配 paid_only（它们在 never 策略下只用免费通道）
	if got := p.SelectChannel("zai", "", nil, ChannelPaidOnly); got == nil || got.UserID != "keyonly" {
		t.Fatalf("paid_only must not match dual-channel account, got %+v", got)
	}
	// 唯一通道耗尽：keyonly 不再被选（status 描述的就是付费通道本身）；
	// 双通道账号不受牵连，仍可被付费阶段选中
	if err := db.SetAccountStatus(id, StatusExhausted, "monitor 额度用完", 0); err != nil {
		t.Fatal(err)
	}
	if got := p.SelectChannel("zai", "", nil, ChannelPaid); got != nil && got.UserID == "keyonly" {
		t.Fatalf("exhausted apikey-only account must be skipped, got %+v", got)
	}
	if got := p.SelectChannel("zai", "", nil, ChannelPaidOnly); got != nil {
		t.Fatalf("exhausted apikey-only account must be skipped (paid_only), got %+v", got)
	}
}

// 通道归因：转发路径标记 paid 后 recordUsage 落 paid，未标记按 free
func TestUsageChannelAttribution(t *testing.T) {
	a := mkDualAccount("attr", StatusActive)
	if a.usageChannelName() != "free" {
		t.Fatalf("default channel = %s, want free", a.usageChannelName())
	}
	a.setUsageChannel(ChannelPaid)
	if a.usageChannelName() != "paid" {
		t.Fatalf("channel after mark = %s, want paid", a.usageChannelName())
	}
}
