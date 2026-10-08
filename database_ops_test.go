package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newOpsTestDB 临时库（与 vault_test.go 同款：包级 vault 种子不得跨测试泄漏）
func newOpsTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ops-test.db")
	t.Cleanup(resetVaultSeed)
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func tsString(t time.Time) string { return t.Format("2006-01-02 15:04:05") }

// R: 保留清扫必须覆盖三张记录表——usage_records 每请求一行、claim_records
// 自动领取每账号每轮一行、plan_run_records 分钟级计划一天 1440 行，只清
// usage_records 另外两张照样无界增长
func TestRetentionPrunesAllRecordTables(t *testing.T) {
	db, _ := newOpsTestDB(t)
	old := tsString(time.Now().AddDate(0, 0, -100))
	fresh := tsString(time.Now())
	seed := func(stmt string, args ...any) {
		if _, err := db.conn.Exec(stmt, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for _, ts := range []string{old, fresh} {
		seed(`INSERT INTO usage_records (created_at, total_tokens) VALUES (?, 1)`, ts)
		seed(`INSERT INTO claim_records (created_at, account_id) VALUES (?, 1)`, ts)
		seed(`INSERT INTO plan_run_records (run_at, plan_id) VALUES (?, 1)`, ts)
	}
	n, err := db.PruneRecords(90)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 3 {
		t.Fatalf("pruned %d rows, want 3 (每表 1 行)", n)
	}
	for _, tbl := range []string{"usage_records", "claim_records", "plan_run_records"} {
		var total int
		if err := db.conn.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&total); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if total != 1 {
			t.Fatalf("%s: %d rows after prune, want 1 (旧行删、新行留)", tbl, total)
		}
	}
}

// R: TTFT 分位不得只看最快尾部——SQL ORDER BY ttft_ms LIMIT cap 取的是
// cap 条最小值，窗口一旦超过 cap，p50/p95 整体向快端漂移
func TestTTFTSamplesNotFastTailOnly(t *testing.T) {
	db, _ := newOpsTestDB(t)
	tx, err := db.conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < 500; i++ {
		if _, err := tx.Exec(`INSERT INTO usage_records (ttft_ms) VALUES (1)`); err != nil {
			t.Fatalf("seed fast: %v", err)
		}
		if _, err := tx.Exec(`INSERT INTO usage_records (ttft_ms) VALUES (1000)`); err != nil {
			t.Fatalf("seed slow: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	since := tsString(time.Now().Add(-time.Hour))
	samples, err := db.ttftSamples(since, 100)
	if err != nil {
		t.Fatalf("ttftSamples: %v", err)
	}
	if len(samples) != 100 {
		t.Fatalf("sample size = %d, want 100", len(samples))
	}
	slow := 0
	for _, v := range samples {
		if v == 1000 {
			slow++
		}
	}
	// 总体一半是 1000ms；只取最快尾部的旧实现 slow 恒为 0。
	// 水库采样下全采到慢样本之外的概率 < 1e-30
	if slow == 0 {
		t.Fatalf("sample drawn only from fast tail: %v", samples)
	}
	for i := 1; i < len(samples); i++ {
		if samples[i-1] > samples[i] {
			t.Fatalf("samples not sorted ascending: %v", samples)
		}
	}
}

// 总体不超过 cap 时水库采样必须全量保留（无偏采样的退化情形）
func TestTTFTSamplesReturnsAllUnderCap(t *testing.T) {
	db, _ := newOpsTestDB(t)
	tx, err := db.conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, v := range []int{7, 3, 9, 1, 5} {
		if _, err := tx.Exec(`INSERT INTO usage_records (ttft_ms) VALUES (?)`, v); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	since := tsString(time.Now().Add(-time.Hour))
	samples, err := db.ttftSamples(since, 100)
	if err != nil {
		t.Fatalf("ttftSamples: %v", err)
	}
	want := []int{1, 3, 5, 7, 9}
	if len(samples) != len(want) {
		t.Fatalf("got %v, want %v", samples, want)
	}
	for i := range want {
		if samples[i] != want[i] {
			t.Fatalf("got %v, want sorted %v", samples, want)
		}
	}
}

// percentile 既有约定：最近秩 idx = int(q*(n-1))（4 样本 p50 落在第 2 位）
func TestPercentileNearestRank(t *testing.T) {
	if got := percentile([]int{10, 20, 30, 40}, 0.5); got != 20 {
		t.Errorf("percentile p50 = %d, want 20", got)
	}
	if got := percentile([]int{10, 20, 30, 40}, 0.95); got != 30 {
		t.Errorf("percentile p95 = %d, want 30", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile of empty = %d, want 0", got)
	}
}

// R: HasResetRecordNear 的 created_at 墙钟回退用"今天"的 UTC 偏移还原 epoch，
// 跨夏令时边界的遗留行偏移差最大 3600s，±900s 窗口漏配（漏配=重复消耗重置）
func TestHasResetRecordNearDSTSkew(t *testing.T) {
	db, _ := newOpsTestDB(t)
	_, offsetNow := time.Now().Zone()
	// 行写入时的偏移比现在大 3600s（夏令时期间写入、标准时间查询）：
	// 用 FixedZone 渲染出"当时的墙钟串"，与机器实际时区无关
	offsetThen := offsetNow + 3600
	dstZone := time.FixedZone("dst", offsetThen)

	insertLegacy := func(resetAt time.Time) {
		wall := resetAt.In(dstZone).Format("2006-01-02 15:04:05")
		if _, err := db.conn.Exec(
			`INSERT INTO claim_records (account_id, task_type, plan_name, success, used_at, created_at)
			 VALUES (1, 'reset', '配额重置(five_hour)', 1, 0, ?)`, wall); err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}
	}

	// 1 小时前：墙钟还原误差 = 3600s，放宽后的 ±5400s 窗口应命中
	resetAt := time.Unix(time.Now().Unix()-3600, 0)
	insertLegacy(resetAt)
	got, err := db.HasResetRecordNear(1, resetAt.Unix(), "five_hour")
	if err != nil {
		t.Fatalf("HasResetRecordNear: %v", err)
	}
	if !got {
		t.Fatalf("DST-skewed legacy row not matched (wall=%s offsetNow=%d)", resetAt, offsetNow)
	}

	// 同一 3600s 偏移差下，比存储行真实时刻再早 2h 的另一次重置不命中：
	// |7200 + 3600| = 10800 ≥ 5400（窗口吸收偏移而非无界）
	farAt := time.Unix(time.Now().Unix()-2*3600, 0)
	insertLegacy(farAt)
	got, err = db.HasResetRecordNear(1, farAt.Unix()-2*3600, "five_hour")
	if err != nil {
		t.Fatalf("HasResetRecordNear(far): %v", err)
	}
	if got {
		t.Fatalf("query 4h from skewed row matched — window too wide")
	}
}

// used_at 精确/邻近分支不受窗口放宽影响（回归保护）
func TestHasResetRecordNearUsedAt(t *testing.T) {
	db, _ := newOpsTestDB(t)
	usedAt := time.Now().Unix() - 3600
	if _, err := db.conn.Exec(
		`INSERT INTO claim_records (account_id, task_type, plan_name, success, used_at)
		 VALUES (1, 'reset', '配额重置(five_hour)', 1, ?)`, usedAt); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 同步行 used_at 是上游时钟，与本地执行行的秒级相等靠不住：±15min 邻近命中
	for _, delta := range []int64{0, 600, -600} {
		got, err := db.HasResetRecordNear(1, usedAt+delta, "five_hour")
		if err != nil {
			t.Fatalf("HasResetRecordNear(+%d): %v", delta, err)
		}
		if !got {
			t.Errorf("used_at delta %ds not matched", delta)
		}
	}
	// 不同 kind 不构成重复
	got, err := db.HasResetRecordNear(1, usedAt, "week")
	if err != nil {
		t.Fatalf("HasResetRecordNear(week): %v", err)
	}
	if got {
		t.Errorf("five_hour record matched as week duplicate")
	}
	// 超出 ±15min 不命中
	got, err = db.HasResetRecordNear(1, usedAt+2*3600, "five_hour")
	if err != nil {
		t.Fatalf("HasResetRecordNear(far): %v", err)
	}
	if got {
		t.Errorf("used_at 2h away matched")
	}
}

// R: -doctor 必须只读——无迁移打开不得建表、不得生成/轮换 vault.key
func TestNewDBWithOptionsDoctorModeLeavesFreshDBUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doctor-fresh.db")
	t.Cleanup(resetVaultSeed)
	db, err := NewDBWithOptions(path, false)
	if err != nil {
		t.Fatalf("NewDBWithOptions(doctor): %v", err)
	}
	defer db.Close()
	var n int
	if err := db.conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table'
		 AND name IN ('accounts','settings','usage_records','claim_records','plan_run_records')`).Scan(&n); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	if n != 0 {
		t.Fatalf("doctor-mode open created %d core tables; want 0 (no schema writes)", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.key")); !os.IsNotExist(err) {
		t.Fatalf("doctor-mode open touched vault.key: %v", err)
	}
}

// doctor 模式对已有库：vault 钥匙要加载（AES 回环探测、账号读取可用），
// 但钥匙状态不变（已有的 keyfile 只认领、不轮换）
func TestNewDBWithOptionsDoctorModeStillLoadsVaultKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doctor-existing.db")
	t.Cleanup(resetVaultSeed)
	full, err := NewDB(path) // 正常打开：建表 + 生成 keyfile
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.key")); err != nil {
		t.Fatalf("expected vault.key after full open: %v", err)
	}
	full.Close()

	db, err := NewDBWithOptions(path, false)
	if err != nil {
		t.Fatalf("reopen doctor-mode: %v", err)
	}
	defer db.Close()
	probe := "doctor-key-probe-烟"
	enc, err := vaultEncrypt(probe)
	if err != nil {
		t.Fatalf("vaultEncrypt: %v", err)
	}
	if got := vaultDecrypt(enc); got != probe {
		t.Fatalf("vault round-trip mismatch: %q", got)
	}
	if _, err := db.ListUsageRecords(1); err != nil {
		t.Fatalf("ListUsageRecords: %v", err)
	}
}
