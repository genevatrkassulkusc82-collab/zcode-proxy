package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newVaultTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vault-test.db")
	t.Cleanup(resetVaultSeed) // 进程内包级种子不得跨测试泄漏
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// resetVaultSeed 模拟进程重启：清掉内存中的种子覆盖，让下一次 NewDB
// 走完整的 ResolveVaultSeed（含 keyfile 读取）
func resetVaultSeed() { setVaultSeedOverride("") }

func TestVaultEncryptOnWriteDecryptOnRead(t *testing.T) {
	db, _ := newVaultTestDB(t)
	a := &Account{
		UserID: "u-vault-1", Email: "vault@test", Provider: "zai", AuthType: "jwt",
		AccessToken: "at-secret", RefreshToken: "rt-secret", ZCodeJWT: "jwt-secret",
		APIKey: "key.secret", UserInfo: `{"email":"vault@test"}`, CredsRaw: `{"zcodejwttoken":"x"}`,
		Status: StatusActive, Enabled: true,
	}
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 原始行必须是密文
	var raw string
	if err := db.conn.QueryRow(`SELECT zcode_jwt FROM accounts WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if !strings.HasPrefix(raw, vaultPrefix) {
		t.Fatalf("stored value not vault-encrypted: %q", raw)
	}
	if strings.Contains(raw, "jwt-secret") {
		t.Fatal("plaintext leaked into stored value")
	}
	// 读回必须透明解密
	got, err := db.GetAccount(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ZCodeJWT != "jwt-secret" || got.AccessToken != "at-secret" || got.CredsRaw != `{"zcodejwttoken":"x"}` {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	// UpdateAccountTokens 同样加密；空值保留旧值
	if err := db.UpdateAccountTokens(id, "at-new", "", "", "", ""); err != nil {
		t.Fatalf("update tokens: %v", err)
	}
	got, err = db.GetAccount(id)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.AccessToken != "at-new" || got.RefreshToken != "rt-secret" {
		t.Fatalf("token update mismatch: at=%q rt=%q", got.AccessToken, got.RefreshToken)
	}
}

func TestVaultMigratesPlaintextRows(t *testing.T) {
	db, path := newVaultTestDB(t)
	// 绕过加密直接插入明文（模拟旧库）
	res, err := db.conn.Exec(`INSERT INTO accounts (user_id, email, provider, auth_type, zcode_jwt, status, enabled)
		VALUES ('u-legacy', 'legacy@test', 'zai', 'jwt', 'legacy-plain-jwt', 'active', 1)`)
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	id, _ := res.LastInsertId()
	db.Close()
	resetVaultSeed() // 模拟进程重启

	// 重新打开触发迁移
	db2, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { db2.Close(); resetVaultSeed() }()
	var raw string
	if err := db2.conn.QueryRow(`SELECT zcode_jwt FROM accounts WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if !strings.HasPrefix(raw, vaultPrefix) {
		t.Fatalf("migration did not encrypt: %q", raw)
	}
	got, err := db2.GetAccount(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ZCodeJWT != "legacy-plain-jwt" {
		t.Fatalf("decrypt after migration mismatch: %q", got.ZCodeJWT)
	}
	// 再开一次：已加密值不重复处理（幂等），且 keyfile 种子被正确采用
	db2.Close()
	resetVaultSeed()
	db3, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	defer func() { db3.Close(); resetVaultSeed() }()
	got, err = db3.GetAccount(id)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got.ZCodeJWT != "legacy-plain-jwt" {
		t.Fatalf("idempotency broken: %q", got.ZCodeJWT)
	}
}

// TestVaultOrphanKeyfileFromCrashedRotation 回归：崩溃轮换留下的孤儿 keyfile
// （密文仍是派生种子加密）必须被删除并重新轮换，绝不能被误采用
func TestVaultOrphanKeyfileFromCrashedRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault-test.db")
	t.Cleanup(resetVaultSeed)
	resetVaultSeed()

	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	// 直接插入"派生种子加密"的行（模拟轮换在重加密前崩溃时的库状态）
	enc, err := EncryptCredential("orphan-scenario-jwt", legacyVaultSeed())
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := db.conn.Exec(`INSERT INTO accounts (user_id, email, provider, auth_type, zcode_jwt, status, enabled)
		VALUES ('u-orphan', 'orphan@test', 'zai', 'jwt', ?, 'active', 1)`,
		vaultPrefix+strings.TrimPrefix(enc, encPrefix)); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	db.Close()

	// 伪造孤儿 keyfile（一把随机无关钥匙，模拟崩溃时已写 keyfile、重加密未提交）
	orphanSeed, err := randomVaultSeed()
	if err != nil {
		t.Fatalf("random seed: %v", err)
	}
	keyFile := filepath.Join(dir, "vault.key")
	if err := os.WriteFile(keyFile, []byte(orphanSeed+"\n"), 0600); err != nil {
		t.Fatalf("write orphan keyfile: %v", err)
	}

	resetVaultSeed()
	db2, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { db2.Close(); resetVaultSeed() }()

	// 孤儿 keyfile 必须已被删除并替换为新轮换的 keyfile
	loaded, ok := loadVaultKeyFile(keyFile)
	if !ok {
		t.Fatalf("rotation did not write a new keyfile")
	}
	if loaded == orphanSeed {
		t.Fatalf("orphan keyfile was adopted instead of removed")
	}
	accounts, err := db2.ListAccounts("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ZCodeJWT != "orphan-scenario-jwt" {
		t.Fatalf("credential not decryptable after orphan-keyfile recovery: %+v", accounts)
	}
}

// TestVaultKeyPersistsAcrossRestart 回归测试：vault.key 必须在重启后被读取采用。
// （历史缺陷：ResolveVaultSeed 只写不读，第二次重启后全部凭证解密失败）
func TestVaultKeyPersistsAcrossRestart(t *testing.T) {
	db, path := newVaultTestDB(t)
	a := &Account{
		UserID: "u-restart", Email: "restart@test", Provider: "zai", AuthType: "jwt",
		ZCodeJWT: "jwt-across-restart", Status: StatusActive, Enabled: true,
	}
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	db.Close()

	// 两次重启：种子都必须从 vault.key 恢复，而非回落到派生种子
	for round := 1; round <= 2; round++ {
		resetVaultSeed()
		db2, err := NewDB(path)
		if err != nil {
			t.Fatalf("reopen round %d: %v", round, err)
		}
		got, err := db2.GetAccount(id)
		db2.Close()
		resetVaultSeed()
		if err != nil {
			t.Fatalf("get round %d: %v", round, err)
		}
		if got.ZCodeJWT != "jwt-across-restart" {
			t.Fatalf("round %d: credential not decryptable after restart (keyfile not adopted?): %q", round, got.ZCodeJWT)
		}
	}
}

// TestVaultMixedKeyConsolidation 回归：派生种子降级态下拒绝新明文写入（P2），
// 但库中可能仍存在旧版本二进制在降级窗口写入的派生种子密文——恢复 keyfile 后
// 必须把这类行并入 keyfile 并采用它，而不是永久停在"钥匙不匹配"告警上。
func TestVaultMixedKeyConsolidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault-test.db")
	t.Cleanup(resetVaultSeed)
	resetVaultSeed()

	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, err := db.UpsertAccount(&Account{
		UserID: "u-mix-a", Email: "a@test", Provider: "zai", AuthType: "jwt",
		ZCodeJWT: "jwt-under-keyfile", Status: StatusActive, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	keyFile := filepath.Join(dir, "vault.key")
	keySeed, ok := loadVaultKeyFile(keyFile)
	if !ok {
		t.Fatalf("keyfile not generated on fresh DB")
	}
	db.Close()

	// 模拟"keyfile 丢失"窗口：删钥匙 → 重开（回落派生种子并告警）→
	// 新明文写入必须被拒绝（P2：派生密钥加密 = 明文等价保护）
	if err := os.Remove(keyFile); err != nil {
		t.Fatalf("remove keyfile: %v", err)
	}
	resetVaultSeed()
	db2, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen without keyfile: %v", err)
	}
	if _, err := db2.UpsertAccount(&Account{
		UserID: "u-mix-b", Email: "b@test", Provider: "zai", AuthType: "jwt",
		ZCodeJWT: "jwt-under-derived", Status: StatusActive, Enabled: true,
	}); err == nil {
		t.Fatal("derived-seed upsert must refuse new credential writes")
	}
	// 旧版本二进制在降级窗口写入的派生种子行（直接构造，验证合并修复能力）
	legacyEnc, err := EncryptCredential("jwt-under-derived", legacyVaultSeed())
	if err != nil {
		t.Fatalf("encrypt legacy-era row: %v", err)
	}
	if _, err := db2.conn.Exec(`INSERT INTO accounts (user_id, email, provider, auth_type, zcode_jwt, status, enabled)
		VALUES ('u-mix-b', 'b@test', 'zai', 'jwt', ?, 'active', 1)`,
		vaultPrefix+strings.TrimPrefix(legacyEnc, encPrefix)); err != nil {
		t.Fatalf("insert legacy-era row: %v", err)
	}
	db2.Close()

	// 恢复原 keyfile → 重开：应触发混合钥匙合并并采用 keyfile
	if err := os.WriteFile(keyFile, []byte(keySeed+"\n"), 0600); err != nil {
		t.Fatalf("restore keyfile: %v", err)
	}
	resetVaultSeed()
	db3, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen with restored keyfile: %v", err)
	}
	defer func() { db3.Close(); resetVaultSeed() }()

	accounts, err := db3.ListAccounts("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]string{}
	for _, a := range accounts {
		got[a.UserID] = a.ZCodeJWT
	}
	if got["u-mix-a"] != "jwt-under-keyfile" {
		t.Fatalf("keyfile-era row not decryptable: %q", got["u-mix-a"])
	}
	if got["u-mix-b"] != "jwt-under-derived" {
		t.Fatalf("derived-era row not consolidated into keyfile: %q", got["u-mix-b"])
	}
	// 新写入必须落在 keyfile 种子下（当前激活种子）
	if _, broken, err := db3.scanVaultCiphertext(keySeed); err != nil || broken != 0 {
		t.Fatalf("post-consolidation scan: broken=%d err=%v", broken, err)
	}
}

func TestVaultDerivedSeedRefusesNewPlaintextWrites(t *testing.T) {
	db, _ := newVaultTestDB(t)
	resetVaultSeed() // 回到派生种子降级态（相当于真钥匙丢失后的进程状态）
	if _, err := vaultEncrypt("plain-secret"); err == nil {
		t.Fatal("derived-seed vaultEncrypt must refuse new plaintext")
	}
	// 降级态迁移必须跳过：不得用可推算密钥加密存量明文
	if err := db.MigrateVault(); err != nil {
		t.Fatalf("MigrateVault under derived seed: %v", err)
	}
	// 派生种子下的解密（迁移源语义）仍然可用
	enc, err := EncryptCredential("legacy-value", legacyVaultSeed())
	if err != nil {
		t.Fatalf("encrypt legacy: %v", err)
	}
	if got := vaultDecrypt(vaultPrefix + strings.TrimPrefix(enc, encPrefix)); got != "legacy-value" {
		t.Fatalf("legacy decrypt under derived seed = %q", got)
	}
}

func TestVaultWrongSecretDegradesReadToEmpty(t *testing.T) {
	db, _ := newVaultTestDB(t)
	if err := db.SetAPIKey("sk-secret-value"); err != nil {
		t.Fatalf("set api key: %v", err)
	}
	if got, err := db.GetAPIKey(); err != nil || got != "sk-secret-value" {
		t.Fatalf("roundtrip = %q, %v", got, err)
	}
	setVaultSeedOverride("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if got, _ := db.GetAPIKey(); got != "" {
		t.Fatalf("wrong-secret read should degrade to empty, got %q", got)
	}
}

func TestVaultSettingsSecretsEncryptedAtRest(t *testing.T) {
	db, _ := newVaultTestDB(t)
	if err := db.SetPasswordHash("$2a$10$testhashvalue0000000000000000000000000000000000000000"); err != nil {
		t.Fatalf("set password hash: %v", err)
	}
	var raw string
	if err := db.conn.QueryRow(`SELECT value FROM settings WHERE key = 'password_hash'`).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if !strings.HasPrefix(raw, vaultPrefix) {
		t.Fatalf("password_hash not vault-encrypted at rest: %q", raw)
	}
	if got, err := db.GetPasswordHash(); err != nil || got != "$2a$10$testhashvalue0000000000000000000000000000000000000000" {
		t.Fatalf("password hash roundtrip = %q, %v", got, err)
	}
}

// TestVaultKeyfileWithStuckRowConverges 回归（P0）：keyfile 可解行与"两种钥匙都
// 解不开"的卡死行混存时，必须采用 keyfile 并在重启后保持收敛——
// 历史缺陷：误判为"真钥匙不匹配"而进入派生降级态，永久卡死且拒绝写入。
func TestVaultKeyfileWithStuckRowConverges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault-test.db")
	t.Cleanup(resetVaultSeed)
	resetVaultSeed()

	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, err := db.UpsertAccount(&Account{
		UserID: "u-k-era", Email: "k@test", Provider: "zai", AuthType: "jwt",
		ZCodeJWT: "jwt-under-keyfile", Status: StatusActive, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	keyFile := filepath.Join(dir, "vault.key")
	keySeed, ok := loadVaultKeyFile(keyFile)
	if !ok {
		t.Fatalf("keyfile missing")
	}
	db.Close()

	// 注入卡死行：用第三把随机钥匙加密（模拟 env 期写入后 env 丢失/损坏）
	stuckSeed, err := randomVaultSeed()
	if err != nil {
		t.Fatalf("stuck seed: %v", err)
	}
	stuckEnc, err := EncryptCredential("jwt-stuck-env-era", stuckSeed)
	if err != nil {
		t.Fatalf("encrypt stuck: %v", err)
	}
	db2, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := db2.conn.Exec(`INSERT INTO accounts (user_id, email, provider, auth_type, zcode_jwt, status, enabled)
		VALUES ('u-stuck', 'stuck@test', 'zai', 'jwt', ?, 'active', 1)`,
		vaultPrefix+strings.TrimPrefix(stuckEnc, encPrefix)); err != nil {
		t.Fatalf("raw insert stuck row: %v", err)
	}
	db2.Close()

	// 恢复 keyfile → 重开：必须采用 keyfile（卡死行如实告警），不得进入降级态
	if err := os.WriteFile(keyFile, []byte(keySeed+"\n"), 0600); err != nil {
		t.Fatalf("restore keyfile: %v", err)
	}
	resetVaultSeed()
	db3, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen with keyfile: %v", err)
	}
	assertState := func(tag string) {
		t.Helper()
		if vaultSeedIsDerived() {
			t.Fatalf("%s: still in derived degraded state — adoption wedge reproduced", tag)
		}
		accounts, err := db3.ListAccounts("")
		if err != nil {
			t.Fatalf("%s: list: %v", tag, err)
		}
		got := map[string]string{}
		for _, a := range accounts {
			got[a.UserID] = a.ZCodeJWT
		}
		if got["u-k-era"] != "jwt-under-keyfile" {
			t.Fatalf("%s: keyfile-era row not decryptable: %q", tag, got["u-k-era"])
		}
		if got["u-stuck"] != "" {
			t.Fatalf("%s: stuck row should be unreadable (its key is gone), got %q", tag, got["u-stuck"])
		}
	}
	assertState("first adoption")

	// 第二次重启：收敛性——同样的分支必须再次采用 keyfile，而不是振荡回降级态
	db3.Close()
	resetVaultSeed()
	db4, err := NewDB(path)
	if err != nil {
		t.Fatalf("second restart: %v", err)
	}
	defer func() { db4.Close(); resetVaultSeed() }()
	db3 = db4
	assertState("second restart")
}
