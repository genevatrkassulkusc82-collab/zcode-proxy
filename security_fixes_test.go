package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- FIX 1：upstream_proxy 凭据静态加密 ----

// TestUpstreamProxyVaultEncryptedAtRest upstream_proxy 落库必须是密文，
// GetSetting / GlobalProxyURL / settings API 层读取均透明解密
func TestUpstreamProxyVaultEncryptedAtRest(t *testing.T) {
	db, _ := newVaultTestDB(t)
	const proxyURL = "socks5://alice:s3cret-pw@proxy.example:1080"
	if err := db.SetSetting("upstream_proxy", proxyURL); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	// 原始行必须是密文（密文在库 = stolen zcode.db 读不出代理凭据）
	var raw string
	if err := db.conn.QueryRow(`SELECT value FROM settings WHERE key = 'upstream_proxy'`).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if !strings.HasPrefix(raw, vaultPrefix) {
		t.Fatalf("upstream_proxy not vault-encrypted at rest: %q", raw)
	}
	if strings.Contains(raw, "s3cret-pw") {
		t.Fatal("proxy credential leaked into stored value")
	}
	// 透明解密读回
	got, err := db.GetSetting("upstream_proxy")
	if err != nil || got != proxyURL {
		t.Fatalf("GetSetting roundtrip = %q, %v", got, err)
	}
	// 出口代理解析走 GetSetting，同样拿到明文
	if u := NewEgressProxy(db).GlobalProxyURL(); u != proxyURL {
		t.Fatalf("GlobalProxyURL = %q, want %q", u, proxyURL)
	}
}

// TestUpstreamProxyPlaintextMigratedOnUpgrade 升级场景：老库里已存在的明文
// upstream_proxy 在下次启动时被 MigrateVault 一次性加密（encrypt-if-plaintext）
func TestUpstreamProxyPlaintextMigratedOnUpgrade(t *testing.T) {
	db, path := newVaultTestDB(t)
	const proxyURL = "socks5://bob:hunter2@proxy.example:1080"
	// 模拟旧版本写入的明文行（绕过 SetSetting 的加密）；initSchema 已插入
	// 默认空值行，用 upsert 覆盖
	if _, err := db.conn.Exec(`INSERT INTO settings (key, value) VALUES ('upstream_proxy', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, proxyURL); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	db.Close()
	resetVaultSeed() // 模拟进程重启

	db2, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { db2.Close(); resetVaultSeed() }()

	var raw string
	if err := db2.conn.QueryRow(`SELECT value FROM settings WHERE key = 'upstream_proxy'`).Scan(&raw); err != nil {
		t.Fatalf("raw read after migration: %v", err)
	}
	if !strings.HasPrefix(raw, vaultPrefix) {
		t.Fatalf("migration did not encrypt upstream_proxy: %q", raw)
	}
	if strings.Contains(raw, "hunter2") {
		t.Fatal("proxy credential survived migration as plaintext")
	}
	if got, err := db2.GetSetting("upstream_proxy"); err != nil || got != proxyURL {
		t.Fatalf("GetSetting after migration = %q, %v", got, err)
	}
}

// TestPutSettingsMaskedProxyRoundTripPreservesAuth PUT 的脱敏回写保护：GET 回的
// 剥离形状被原样回传时不得当作新值落库（会静默抹掉已存 user:pass）；真正的
// 新值（含新凭据）照常替换
func TestPutSettingsMaskedProxyRoundTripPreservesAuth(t *testing.T) {
	db, _ := newVaultTestDB(t)
	const stored = "socks5://alice:s3cret@proxy.example:1080"
	if err := db.SetSetting("upstream_proxy", stored); err != nil {
		t.Fatalf("set: %v", err)
	}
	s := &APIServer{db: db, zapi: &ZCodeAPI{appVersion: "test"}, cfg: &FileConfig{}}

	put := func(v string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"upstream_proxy": v})
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(string(body)))
		s.handlePutSettings(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %q: status = %d body = %s", v, w.Code, w.Body.String())
		}
	}

	// 剥离形状回传 = "没改"：库存值必须原样保留（含凭据）
	put("socks5://proxy.example:1080")
	if got, _ := db.GetSetting("upstream_proxy"); got != stored {
		t.Fatalf("masked round-trip destroyed stored credentials: %q", got)
	}
	// 全新值照常替换
	put("http://dave:newpw@proxy2.example:8080")
	if got, _ := db.GetSetting("upstream_proxy"); got != "http://dave:newpw@proxy2.example:8080" {
		t.Fatalf("genuine new value not stored: %q", got)
	}
}

// TestHandleGetSettingsMasksUpstreamProxyAuth settings API 对 upstream_proxy
// 只回剥离凭据的显示形状（host:port + has_auth 标记）：内嵌 user:pass 是凭据，
// 纯 session 不得读到（F6 契约）；对其余机密键保持脱敏
func TestHandleGetSettingsMasksUpstreamProxyAuth(t *testing.T) {
	db, _ := newVaultTestDB(t)
	const proxyURL = "http://carol:pw@proxy.example:8080"
	if err := db.SetSetting("upstream_proxy", proxyURL); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := db.SetAPIKey("sk-0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatalf("set api key: %v", err)
	}
	s := &APIServer{db: db, zapi: &ZCodeAPI{appVersion: "test"}, cfg: &FileConfig{}}
	w := httptest.NewRecorder()
	s.handleGetSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["upstream_proxy"] != "http://proxy.example:8080" {
		t.Fatalf("upstream_proxy = %q, want stripped %q", out["upstream_proxy"], "http://proxy.example:8080")
	}
	if out["upstream_proxy_has_auth"] != "1" {
		t.Fatalf("upstream_proxy_has_auth = %q, want 1", out["upstream_proxy_has_auth"])
	}
	if strings.Contains(w.Body.String(), "carol") || strings.Contains(w.Body.String(), "pw@") {
		t.Fatal("proxy credentials leaked into settings response")
	}
	if _, ok := out["password_hash"]; ok {
		t.Fatal("password_hash must not appear in settings response")
	}
	if out["has_api_key"] != "1" {
		t.Fatalf("has_api_key = %q, want 1", out["has_api_key"])
	}
	if _, ok := out["api_key"]; ok {
		t.Fatal("api_key must not appear in settings response")
	}
}

// ---- FIX 2：根 Key 仅凭 session 不可读 ----

func newAuthFixtures(t *testing.T) (*AuthManager, *DB) {
	t.Helper()
	db, _ := newVaultTestDB(t)
	am := &AuthManager{db: db, sessions: make(map[string]*sessionEntry), failures: make(map[string]*loginFail)}
	return am, db
}

// TestHandleGetAPIKeyMasksKey GET /api/settings/api-key 只回存在性与脱敏形状，
// 响应体任何位置不得出现明文 Key
func TestHandleGetAPIKeyMasksKey(t *testing.T) {
	am, db := newAuthFixtures(t)
	key := GenerateAPIKey()
	if err := db.SetAPIKey(key); err != nil {
		t.Fatalf("set api key: %v", err)
	}
	w := httptest.NewRecorder()
	am.HandleGetAPIKey(w, httptest.NewRequest(http.MethodGet, "/api/settings/api-key", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, key) {
		t.Fatalf("plaintext key leaked via GET: %s", body)
	}
	var out struct {
		HasAPIKey    bool   `json:"has_api_key"`
		APIKeyMasked string `json:"api_key_masked"`
		APIKey       string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.HasAPIKey {
		t.Fatal("has_api_key = false, want true")
	}
	if out.APIKey != "" {
		t.Fatalf("api_key field must be absent, got %q", out.APIKey)
	}
	if out.APIKeyMasked == "" || out.APIKeyMasked == key {
		t.Fatalf("bad masked form: %q", out.APIKeyMasked)
	}
}

// TestHandleRevealAPIKeyStepUp POST /api/settings/api-key/reveal：
// 正确口令回明文；错误口令 401 且累计失败；5 次错误后 429（含正确口令——
// reveal 无治疗性旁路，与导出同一模式）
func TestHandleRevealAPIKeyStepUp(t *testing.T) {
	am, db := newAuthFixtures(t)
	key := GenerateAPIKey()
	if err := db.SetAPIKey(key); err != nil {
		t.Fatalf("set api key: %v", err)
	}
	hash, err := hashPassword("admin-pass-123")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := db.SetPasswordHash(hash); err != nil {
		t.Fatalf("set hash: %v", err)
	}

	post := func(pw string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"verify_password": pw})
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/settings/api-key/reveal", strings.NewReader(string(body)))
		r.RemoteAddr = "10.9.9.9:4444"
		am.HandleRevealAPIKey(w, r)
		return w
	}

	// 错误口令：401，不回 Key
	if w := post("wrong-pass"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", w.Code)
	}
	if w := post("wrong-pass"); strings.Contains(w.Body.String(), key) {
		t.Fatal("key must not be revealed on failed verification")
	}
	// 正确口令：200 + 明文 Key
	w := post("admin-pass-123")
	if w.Code != http.StatusOK {
		t.Fatalf("correct password status = %d, body = %s", w.Code, w.Body.String())
	}
	var out struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.APIKey != key {
		t.Fatalf("revealed key = %q, want %q", out.APIKey, key)
	}

	// 5 次错误 → 锁定：正确口令也 429（无旁路），不回 Key
	am.clearLoginFail("10.9.9.10|api-key-reveal")
	for i := 0; i < 5; i++ {
		if w := post("wrong"); w.Code != http.StatusUnauthorized {
			t.Fatalf("fail %d: status = %d, want 401", i+1, w.Code)
		}
	}
	if w := post("admin-pass-123"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("locked reveal status = %d, want 429", w.Code)
	} else if strings.Contains(w.Body.String(), key) {
		t.Fatal("key must not be revealed while rate-limited")
	}
}

// TestHandleGetAPIKeyUnset 空库时 GET 只报 has_api_key=false
func TestHandleGetAPIKeyUnset(t *testing.T) {
	am, _ := newAuthFixtures(t)
	w := httptest.NewRecorder()
	am.HandleGetAPIKey(w, httptest.NewRequest(http.MethodGet, "/api/settings/api-key", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["has_api_key"] != false {
		t.Fatalf("has_api_key = %v, want false", out["has_api_key"])
	}
	if _, ok := out["api_key"]; ok {
		t.Fatal("api_key field must be absent")
	}
}

// ---- FIX 3：引导口令落 0600 文件而非日志 ----

// TestInitialAdminPasswordWrittenToFile 全新库引导时口令写入数据目录 0600 文件，
// 内容即生效口令（verifyPassword 通过）
func TestInitialAdminPasswordWrittenToFile(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(filepath.Join(dir, "auth-test.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer func() { db.Close(); resetVaultSeed() }()

	am := NewAuthManager(db, "")
	if am.fallbackPwd == "" {
		t.Fatal("bootstrap did not generate an initial password")
	}
	path := filepath.Join(dir, initialAdminPasswordFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("password file missing: %v", err)
	}
	pw := strings.TrimSpace(string(raw))
	if pw != am.fallbackPwd {
		t.Fatalf("file content mismatch: file=%q fallback=%q", pw, am.fallbackPwd)
	}
	if !am.verifyPassword(pw) {
		t.Fatal("password from file does not verify against the stored hash")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("password file perm = %v, want -r------- (0600)", fi.Mode().Perm())
	}
	// vaultDataDir 与 keyfile 同目录（= 数据目录）的一致性。
	// macOS 的 /var/folders 是 /private/var/folders 的符号链接：SQLite 打开后
	// PRAGMA database_list 返回的是解析后的真实路径，比较前先同样解析
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	if got := vaultDataDir(db); got != resolved {
		t.Fatalf("vaultDataDir = %q, want %q", got, resolved)
	}
}

// ---- FIX 4：登录锁定的治疗性旁路 ----

// TestLoginLockoutTherapeuticBypass 锁定中正确口令仍可登录并清除失败状态；
// 错误口令按原样返回锁定等待且不续期锁定
func TestLoginLockoutTherapeuticBypass(t *testing.T) {
	// 收缩验证冷却：锁定中错误猜测会消耗一次验证预算，正确口令紧随其后
	// 本应等满冷却（生产 30s）；测试用 10ms 冷却有界等待即可观察到放行
	origCooldown := loginBypassCooldown
	loginBypassCooldown = 10 * time.Millisecond
	defer func() { loginBypassCooldown = origCooldown }()
	am, db := newAuthFixtures(t)
	hash, err := hashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := db.SetPasswordHash(hash); err != nil {
		t.Fatalf("set hash: %v", err)
	}
	const ip = "203.0.113.7"
	const user = "admin"
	rateKey := "login|" + ip + "|" + user

	// 5 次错误 → 锁定
	for i := 0; i < loginMaxFails; i++ {
		if _, ok, wait := am.Login(user, "nope-"+string(rune('a'+i)), ip); ok || wait != 0 {
			t.Fatalf("pre-lock attempt %d: ok=%v wait=%v", i+1, ok, wait)
		}
	}
	if wait := am.checkLoginRate(rateKey); wait <= 0 {
		t.Fatal("account not locked after 5 failures")
	}

	// 锁定中错误口令：返回锁定等待；被验证的猜测必须计入失败并续期锁定（F2 契约：
	// 否则锁定窗口内可无限爆破）。验证次数受 loginBypassCooldown 约束
	oldCount := am.failures[rateKey].count
	_, ok, wait := am.Login(user, "still-wrong", ip)
	if ok || wait <= 0 {
		t.Fatalf("locked wrong login: ok=%v wait=%v, want ok=false wait>0", ok, wait)
	}
	if am.failures[rateKey].count <= oldCount {
		t.Fatalf("verified wrong guess during lock not counted: count %d→%d, want growth", oldCount, am.failures[rateKey].count)
	}

	// 锁定中正确口令：正常登录（治疗性旁路）。刚被验证过一次错误猜测时，
	// 验证预算冷却可能要求重试——收缩冷却后有界重试，仍须登录成功
	token, ok, wait := "", false, time.Duration(1)
	for attempt := 0; attempt < 3 && !ok; attempt++ {
		token, ok, wait = am.Login(user, "correct-horse-battery", ip)
		if !ok {
			if wait <= 0 {
				t.Fatalf("therapeutic attempt %d: not ok but wait=%v", attempt+1, wait)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !ok || token == "" {
		t.Fatalf("therapeutic login failed: ok=%v token-empty=%v wait=%v", ok, token == "", wait)
	}
	if !am.IsValid(token) {
		t.Fatal("therapeutic login token invalid")
	}
	if _, exists := am.failures[rateKey]; exists {
		t.Fatal("failure state not cleared after successful login")
	}

	// 清除后错误口令从头计数：不再处于锁定
	if _, ok, wait := am.Login(user, "wrong-again", ip); ok || wait != 0 {
		t.Fatalf("post-clear wrong login: ok=%v wait=%v, want ok=false wait=0", ok, wait)
	}

	am.Logout(token)
}

// TestLoginTherapeuticBypassRequiresUsername 锁定旁路同样要求用户名正确：
// 攻击者自己的 ip|user 键被锁定后，即便拿正确口令也必须保持锁定
func TestLoginTherapeuticBypassRequiresUsername(t *testing.T) {
	am, db := newAuthFixtures(t)
	hash, err := hashPassword("right-pass")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := db.SetPasswordHash(hash); err != nil {
		t.Fatalf("set hash: %v", err)
	}
	const ip = "203.0.113.8"
	for i := 0; i < loginMaxFails; i++ {
		am.Login("intruder", "bad", ip)
	}
	rateKey := "login|" + ip + "|intruder"
	if wait := am.checkLoginRate(rateKey); wait <= 0 {
		t.Fatal("precondition failed: intruder key not locked")
	}
	if _, ok, wait := am.Login("intruder", "right-pass", ip); ok || wait <= 0 {
		t.Fatalf("wrong username must stay locked: ok=%v wait=%v", ok, wait)
	}
}
