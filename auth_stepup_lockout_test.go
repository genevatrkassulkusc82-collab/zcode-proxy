package main

// 冻结测试（2026-10-01 评审修复波）：锁定 ca47015 评审发现 F1/F2 的期望行为。
// 修复方不得修改本文件；只允许改生产代码让它们变绿。
// F1：根 Key 生成/命名 Key 创建必须口令步进（stolen session 不得铸新 Key）。
// F2：锁定期口令猜测必须有界且计入失败（锁定旁路不得变成无限爆破窗口）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- F1：generate / create-key 的口令步进 ----

// TestGenerateAPIKeyRequiresStepUp POST /api/settings/api-key/generate 必须
// 与 reveal 同规：verify_password 缺失/错误 → 401 且不铸 Key（旧 Key 保持有效），
// 正确 → 200 + 新明文。ca47015 下 session 单独即可拿明文根 Key（评审 F1）。
func TestGenerateAPIKeyRequiresStepUp(t *testing.T) {
	am, db := newAuthFixtures(t)
	oldKey := GenerateAPIKey()
	if err := db.SetAPIKey(oldKey); err != nil {
		t.Fatalf("set api key: %v", err)
	}
	hash, err := hashPassword("step-up-pass-9")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := db.SetPasswordHash(hash); err != nil {
		t.Fatalf("set hash: %v", err)
	}

	post := func(pw string, withBody bool) *httptest.ResponseRecorder {
		t.Helper()
		var body *strings.Reader
		if withBody {
			b, _ := json.Marshal(map[string]string{"verify_password": pw})
			body = strings.NewReader(string(b))
		} else {
			body = strings.NewReader("{}")
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/settings/api-key/generate", body)
		r.RemoteAddr = "10.9.9.44:4444"
		am.HandleGenerateAPIKey(w, r)
		return w
	}

	// 无口令（纯 session）：401，不回 Key，旧 Key 未被轮换
	w := post("", false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("generate without password: status = %d, want 401; body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-") {
		t.Fatalf("no key material may appear on failed step-up: %s", w.Body.String())
	}
	if got, _ := db.GetAPIKey(); got != oldKey {
		t.Fatal("failed step-up must not rotate the root key")
	}

	// 错误口令：401，同样不轮换
	if w := post("wrong-pass", true); w.Code != http.StatusUnauthorized {
		t.Fatalf("generate wrong password: status = %d, want 401", w.Code)
	}
	if got, _ := db.GetAPIKey(); got != oldKey {
		t.Fatal("wrong-password step-up must not rotate the root key")
	}

	// 正确口令：200 + 新明文 Key，且落库生效
	w = post("step-up-pass-9", true)
	if w.Code != http.StatusOK {
		t.Fatalf("generate correct password: status = %d, body = %s", w.Code, w.Body.String())
	}
	var out struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.APIKey == "" || out.APIKey == oldKey {
		t.Fatalf("generated key = %q, want fresh key", out.APIKey)
	}
	if got, _ := db.GetAPIKey(); got != out.APIKey {
		t.Fatal("generated key must be persisted")
	}
}

// TestCreateGatewayKeyRequiresStepUp POST /api/keys（命名网关 Key 创建）同一
// 步进契约：无/错口令 401 且不落库（空 models + 0 限额的命名 Key = 无限制 API
// 访问，stolen session 不得铸出来），正确口令才创建。
func TestCreateGatewayKeyRequiresStepUp(t *testing.T) {
	am, db := newAuthFixtures(t)
	hash, err := hashPassword("step-up-pass-7")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := db.SetPasswordHash(hash); err != nil {
		t.Fatalf("set hash: %v", err)
	}
	_ = am
	s := &APIServer{db: db}

	post := func(pw string, withBody bool) *httptest.ResponseRecorder {
		t.Helper()
		var body *strings.Reader
		if withBody {
			b, _ := json.Marshal(map[string]string{
				"name": "frozen-key", "verify_password": pw,
			})
			body = strings.NewReader(string(b))
		} else {
			body = strings.NewReader(`{"name":"frozen-key"}`)
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/keys", body)
		r.RemoteAddr = "10.9.9.45:4444"
		s.handleCreateKey(w, r)
		return w
	}

	// 无口令：401，且库里不落 Key
	if w := post("", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("create-key without password: status = %d, want 401; body = %s", w.Code, w.Body.String())
	}
	if keys, _ := db.ListGatewayKeys(); len(keys) != 0 {
		t.Fatalf("failed step-up must not create keys, found %d", len(keys))
	}

	// 错误口令：401，不落库
	if w := post("wrong-pass", true); w.Code != http.StatusUnauthorized {
		t.Fatalf("create-key wrong password: status = %d, want 401", w.Code)
	}
	if keys, _ := db.ListGatewayKeys(); len(keys) != 0 {
		t.Fatalf("wrong-password step-up must not create keys, found %d", len(keys))
	}

	// 正确口令：200 + 创建成功
	w := post("step-up-pass-7", true)
	if w.Code != http.StatusOK {
		t.Fatalf("create-key correct password: status = %d, body = %s", w.Code, w.Body.String())
	}
	keys, _ := db.ListGatewayKeys()
	if len(keys) != 1 {
		t.Fatalf("keys after create = %d, want 1", len(keys))
	}
}

// ---- F2：锁定期猜测的有界性与失败计数 ----

// TestLoginBypassWrongGuessCountsAsFailure 锁定中被实际验证过的错误口令必须
// 记入失败（并升级锁定）。ca47015 的锁定旁路分支不记失败 → 锁定窗口内无限
// 爆破（评审 F2）。
func TestLoginBypassWrongGuessCountsAsFailure(t *testing.T) {
	am, db := newAuthFixtures(t)
	hash, err := hashPassword("therapeutic-pass")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := db.SetPasswordHash(hash); err != nil {
		t.Fatalf("set hash: %v", err)
	}
	const ip = "203.0.113.41"
	const user = "admin"
	rateKey := "login|" + ip + "|" + user

	// 5 次错误 → 锁定
	for i := 0; i < loginMaxFails; i++ {
		am.Login(user, "wrong-"+string(rune('a'+i)), ip)
	}
	if am.checkLoginRate(rateKey) <= 0 {
		t.Fatal("precondition: account not locked after 5 failures")
	}
	countBefore := am.failures[rateKey].count

	// 锁定中错误口令：返回锁定等待是预期的，但已被验证的猜测必须计入失败
	_, ok, wait := am.Login(user, "still-wrong", ip)
	if ok || wait <= 0 {
		t.Fatalf("locked wrong login: ok=%v wait=%v, want ok=false wait>0", ok, wait)
	}
	if got := am.failures[rateKey].count; got <= countBefore {
		t.Fatalf("verified wrong guess during lock not counted: count %d→%d, want >%d",
			countBefore, got, countBefore)
	}
}

// TestLoginBypassGuessesBounded 锁定窗口内的错误猜测必须有界：20 次快速连打
// 最多允许 ~1-2 次真实口令验证（每冷却窗口一次），其余在验证前即被拒。
// 下界（≥1 次计数）锁定"猜测必须计入失败"，上界锁定"不得 20 次全验证"。
// 允许实现引入验证冷却（如 loginBypassCooldown），但不得依赖其具体时长：
// 快速连打内冷却必然未过期。
func TestLoginBypassGuessesBounded(t *testing.T) {
	am, db := newAuthFixtures(t)
	hash, err := hashPassword("therapeutic-pass-2")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := db.SetPasswordHash(hash); err != nil {
		t.Fatalf("set hash: %v", err)
	}
	const ip = "203.0.113.42"
	const user = "admin"
	rateKey := "login|" + ip + "|" + user

	for i := 0; i < loginMaxFails; i++ {
		am.Login(user, "wrong-"+string(rune('a'+i)), ip)
	}
	if am.checkLoginRate(rateKey) <= 0 {
		t.Fatal("precondition: account not locked after 5 failures")
	}
	countBefore := am.failures[rateKey].count

	for i := 0; i < 20; i++ {
		am.Login(user, "burst-wrong", ip)
	}
	grew := am.failures[rateKey].count - countBefore
	if grew < 1 {
		t.Fatalf("burst during lock recorded %d new failures, want >=1 (guesses must count)", grew)
	}
	if grew > 2 {
		t.Fatalf("burst during lock recorded %d new failures, want <=2 (20 guesses must not each verify)", grew)
	}
	// 连打之后仍在锁定（有界猜测不等于解锁）
	if am.checkLoginRate(rateKey) <= 0 {
		t.Fatal("lock must persist through the guess burst")
	}
}

// TestLoginTherapeuticBypassPreserved 锁定旁路的既有契约必须保留：锁定后
// 正确口令仍能立刻登录并清空失败状态（防第三方 5 次假登录锁死真管理员）。
// 本测试在 ca47015 即为绿——是不许被 F2 修复破坏的保留属性。
func TestLoginTherapeuticBypassPreserved(t *testing.T) {
	am, db := newAuthFixtures(t)
	hash, err := hashPassword("therapeutic-pass-3")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := db.SetPasswordHash(hash); err != nil {
		t.Fatalf("set hash: %v", err)
	}
	const ip = "203.0.113.43"
	const user = "admin"
	rateKey := "login|" + ip + "|" + user

	for i := 0; i < loginMaxFails; i++ {
		am.Login(user, "wrong-"+string(rune('a'+i)), ip)
	}
	if am.checkLoginRate(rateKey) <= 0 {
		t.Fatal("precondition: account not locked after 5 failures")
	}

	// 锁定中正确口令：必须登录成功（允许返回 wait=0 的直接成功；
	// 若实现引入验证冷却，则首次尝试可能被拒——此时重试若干次仍必须成功）
	token, ok, wait := "", false, time.Duration(1)
	for attempt := 0; attempt < 3 && !ok; attempt++ {
		token, ok, wait = am.Login(user, "therapeutic-pass-3", ip)
		if !ok {
			if wait <= 0 {
				t.Fatalf("therapeutic attempt %d: not ok but wait=%v", attempt+1, wait)
			}
			time.Sleep(2 * time.Millisecond) // 冷却实现下短暂重试；不依赖其具体时长
		}
	}
	if !ok || token == "" {
		t.Fatal("therapeutic bypass broken: correct password could not log in while locked")
	}
	if !am.IsValid(token) {
		t.Fatal("therapeutic login token invalid")
	}
	if _, exists := am.failures[rateKey]; exists {
		t.Fatal("failure state not cleared after successful login")
	}
	am.Logout(token)
}
