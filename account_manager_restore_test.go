package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- RestoreLocalFromSnapshot 密文预检（F4）----
// 还原目标恒为主 home，而多实例快照的 enc:v1 密文以实例目录推导密钥；
// 预检失败必须整体拒绝、一个字节都不写。测试全程 t.TempDir()，
// 且 HOME 指向临时目录，绝不触碰真实 ~/.zcode。

// restoreTestEnv 把还原目标 home 指到临时目录，并清空 ZCODE_CREDENTIAL_SECRET
// 使密钥严格按 home 路径派生——不同路径即不同密钥，正是多实例差异的来源。
func restoreTestEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ZCODE_CREDENTIAL_SECRET", "")
	return home
}

// newRestoreManager 建临时库并写入带凭证快照的账号
func newRestoreManager(t *testing.T, snapshot map[string]string) (*AccountManager, int64) {
	t.Helper()
	db, err := NewDB(filepath.Join(t.TempDir(), "restore-test.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	id, err := db.UpsertAccount(&Account{
		UserID: "restore-test", Email: "restore@test", Provider: "zai", AuthType: "jwt",
		ZCodeJWT: "jwt", Status: StatusActive, Enabled: true, CredsRaw: string(raw),
	})
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	return &AccountManager{db: db}, id
}

// seedRestoreTargets 预置目标 home 现网凭证（before-* 标记，供断言未被改写）
func seedRestoreTargets(t *testing.T, f localClientFiles) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.credentials), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for path, content := range map[string]string{f.credentials: "before-creds", f.config: "before-config"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
}

// assertRestoreTargetsIntact 断言拒绝还原后目标文件原封未动（无任何写入）
func assertRestoreTargetsIntact(t *testing.T, f localClientFiles) {
	t.Helper()
	for path, want := range map[string]string{f.credentials: "before-creds", f.config: "before-config"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("target %s should be untouched: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("target %s written by refused restore: got %q want %q", path, got, want)
		}
	}
}

// TestRestoreRefusesForeignEncryptedCredentials RED：credentials.json 里的 enc:v1
// 值用别的 home 密钥（多实例实例目录）加密时必须整体拒绝、不写任何文件。
func TestRestoreRefusesForeignEncryptedCredentials(t *testing.T) {
	home := restoreTestEnv(t)
	f := localClientFilesFor(home)
	seedRestoreTargets(t, f)

	foreignSecret := DefaultCredentialSecret(t.TempDir()) // 另一实例目录 → 另一把密钥
	foreignEnc, err := EncryptCredential("foreign-jwt", foreignSecret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	m, id := newRestoreManager(t, map[string]string{
		"credentials.json": `{"zcodejwttoken":"` + foreignEnc + `"}`,
	})

	err = m.RestoreLocalFromSnapshot(id)
	if err == nil {
		t.Fatal("foreign-encrypted credentials.json must be refused")
	}
	if !strings.Contains(err.Error(), "credentials.json") || !strings.Contains(err.Error(), "zcodejwttoken") {
		t.Fatalf("error must name file and field: %v", err)
	}
	assertRestoreTargetsIntact(t, f)
}

// TestRestoreAcceptsLocalEncryptedAndPlaintext GREEN：目标 home 密钥可解的值
// 与明文值都照常还原。
func TestRestoreAcceptsLocalEncryptedAndPlaintext(t *testing.T) {
	home := restoreTestEnv(t)
	f := localClientFilesFor(home)
	seedRestoreTargets(t, f)

	secret := DefaultCredentialSecret(home) // 目标 home 密钥
	encVal, err := EncryptCredential("local-jwt", secret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	credContent := `{"zcodejwttoken":"` + encVal + `","custom_key":"plaintext-value"}`
	cfgContent := `{"provider":{"builtin:zai-coding-plan":{"enabled":true,"options":{"apiKey":"{id}.{secret}"}}}}`
	m, id := newRestoreManager(t, map[string]string{
		"credentials.json": credContent,
		"config.json":      cfgContent,
	})

	if err := m.RestoreLocalFromSnapshot(id); err != nil {
		t.Fatalf("restore should succeed: %v", err)
	}
	for path, want := range map[string]string{f.credentials: credContent, f.config: cfgContent} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("restored %s mismatch:\n got %q\nwant %q", path, got, want)
		}
	}
}

// TestRestoreRefusesForeignEncryptedConfigAPIKey RED：config.json 的 provider
// apiKey 为外实例密钥密文时必须整体拒绝。
func TestRestoreRefusesForeignEncryptedConfigAPIKey(t *testing.T) {
	home := restoreTestEnv(t)
	f := localClientFilesFor(home)
	seedRestoreTargets(t, f)

	foreignSecret := DefaultCredentialSecret(t.TempDir())
	foreignEnc, err := EncryptCredential("{id}.{foreign-secret}", foreignSecret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	cfgContent := `{"provider":{"builtin:zai-coding-plan":{"enabled":true,"options":{"apiKey":"` + foreignEnc + `"}}}}`
	m, id := newRestoreManager(t, map[string]string{
		"credentials.json": `{"custom_key":"plaintext-value"}`,
		"config.json":      cfgContent,
	})

	err = m.RestoreLocalFromSnapshot(id)
	if err == nil {
		t.Fatal("foreign-encrypted config.json apiKey must be refused")
	}
	if !strings.Contains(err.Error(), "config.json") || !strings.Contains(err.Error(), "builtin:zai-coding-plan") {
		t.Fatalf("error must name file and provider: %v", err)
	}
	assertRestoreTargetsIntact(t, f)
}

// TestRestoreRefusesMalformedInnerJSON 内层 JSON 解析不了的快照无从确认密文
// 归属，必须拒绝还原。
func TestRestoreRefusesMalformedInnerJSON(t *testing.T) {
	home := restoreTestEnv(t)
	f := localClientFilesFor(home)
	seedRestoreTargets(t, f)

	m, id := newRestoreManager(t, map[string]string{
		"credentials.json": `{"zcodejwttoken": "enc:v1:unterminated`,
		"config.json":      `{"provider":`,
	})

	err := m.RestoreLocalFromSnapshot(id)
	if err == nil {
		t.Fatal("malformed inner JSON must be refused")
	}
	if !strings.Contains(err.Error(), "快照") {
		t.Fatalf("error must name the snapshot: %v", err)
	}
	assertRestoreTargetsIntact(t, f)
}
