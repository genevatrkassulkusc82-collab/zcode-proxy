package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ---- Web 管理界面认证 ----
// /api/* 使用 session 认证（/api/login 除外）
// /v1/* 使用 API Key 认证（sk- 前缀）
// /web 页面与 /oauth/callback 始终可访问

const (
	sessionCookieName = "zcode_session"
	sessionExpiry     = 24 * time.Hour
)

type sessionEntry struct {
	username  string
	createdAt time.Time
	expiresAt time.Time
}

// AuthManager 认证管理器
type AuthManager struct {
	mu          sync.RWMutex
	sessions    map[string]*sessionEntry
	fallbackPwd string
	db          *DB

	failMu   sync.Mutex
	failures map[string]*loginFail // 登录失败限速：key = ip|user

	gwRPM gwRPMTracker // 命名网关 Key 的 RPM 滑动窗口（R1）
}

type loginFail struct {
	count       int
	lockedUntil time.Time
}

const (
	loginMaxFails = 5
	loginLockBase = 60 * time.Second
	loginLockMax  = 30 * time.Minute
)

// pwdAlphabet 无歧义随机口令字母表（去除 0O1lI 等易混淆字符）
const pwdAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// generateRandomPassword 生成 n 位 crypto/rand 随机口令
func generateRandomPassword(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = pwdAlphabet[int(v)%len(pwdAlphabet)]
	}
	return string(out), nil
}

// NewAuthManager 创建认证管理器：优先 ZCODE_WEB_PASS；全新数据库（无口令哈希）
// 生成随机管理口令，bcrypt 落库并打印一次。不再存在硬编码默认口令。
func NewAuthManager(db *DB, password string) *AuthManager {
	fallbackPwd := password
	if fallbackPwd == "" && db != nil {
		hashPresent, perr := db.HasSetting("password_hash")
		stored, _ := db.GetPasswordHash()
		if perr == nil && hashPresent && stored == "" {
			// 哈希行存在但当前钥匙解不开（错误 env / 换钥匙后启动）：
			// 覆盖会把原口令哈希永久销毁——跳过引导，保持 env 兜底并告警
			log.Printf("[auth] WARNING: password hash exists but cannot be decrypted with the active vault key; " +
				"password bootstrap skipped — restore the key or set ZCODE_PROXY_VAULT_SECRET. " +
				"ZCODE_WEB_PASS (if set) still works")
		} else if stored == "" {
			// 全新数据库：随机生成初始管理口令（is_default_password=1，UI 会提示修改）
			if pw, err := generateRandomPassword(20); err == nil {
				fallbackPwd = pw
				if hash, err := hashPassword(pw); err != nil {
					log.Printf("[auth] persist initial admin password hash failed: %v", err)
				} else if err := db.SetPasswordHash(hash); err != nil {
					log.Printf("[auth] persist initial admin password hash failed: %v", err)
				} else {
					db.SetDefaultPasswordFlag(true)
				}
				log.Printf("[auth] initial admin password: %s (change it in the web UI)", pw)
			} else {
				log.Printf("[auth] generate admin password failed: %v", err)
			}
		}
	}
	return &AuthManager{
		sessions:    make(map[string]*sessionEntry),
		fallbackPwd: fallbackPwd,
		db:          db,
		failures:    make(map[string]*loginFail),
	}
}

// maxPasswordBytes bcrypt 只取前 72 字节，超长口令在 x/crypto 中直接报错；
// 请求路径上必须在哈希前拒绝，而不是让服务进程退出
const maxPasswordBytes = 72

// hashPassword bcrypt 哈希（新口令）；输入超长返回错误而非崩溃
func hashPassword(pwd string) (string, error) {
	if len(pwd) > maxPasswordBytes {
		return "", fmt.Errorf("password exceeds %d bytes", maxPasswordBytes)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pwd), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// legacyHash 旧版无盐 SHA-256（仅用于透明迁移比对）
func legacyHash(pwd string) string {
	h := sha256.Sum256([]byte(pwd))
	return hex.EncodeToString(h[:])
}

// cookieSecureOverride ZCODE_COOKIE_SECURE=1/true/on/yes 时强制会话 cookie Secure 位
// （TLS 由反向代理终止时 r.TLS 恒为 nil，启发式探测不到）
func cookieSecureOverride() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ZCODE_COOKIE_SECURE")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// verifyPassword 校验口令；旧 SHA-256 哈希命中后透明升级为 bcrypt
func (am *AuthManager) verifyPassword(pwd string) bool {
	stored := ""
	if am.db != nil {
		stored, _ = am.db.GetPasswordHash()
	}
	if stored != "" {
		if strings.HasPrefix(stored, "$2") {
			return bcrypt.CompareHashAndPassword([]byte(stored), []byte(pwd)) == nil
		}
		// 常数时间比对：旧哈希为无盐 SHA-256，短路比较会泄露前缀匹配长度
		if subtle.ConstantTimeCompare([]byte(legacyHash(pwd)), []byte(stored)) == 1 {
			// 透明升级为 bcrypt；超长口令无法哈希时保持旧哈希（下次登录再试），
			// 登录本身仍以 legacy 比对结果为准
			if hash, err := hashPassword(pwd); err == nil {
				am.db.SetPasswordHash(hash)
				// 仍是缺省口令（admin/admin 老库）：标记之，UI 会提示修改
				if pwd == "admin" {
					am.db.SetDefaultPasswordFlag(true)
				}
				log.Printf("[auth] password hash migrated to bcrypt")
			} else {
				log.Printf("[auth] bcrypt migration skipped: %v", err)
			}
			return true
		}
		return false
	}
	// 兜底：环境变量口令（常数时间比较，长度不等直接拒绝）
	if am.fallbackPwd == "" || len(pwd) != len(am.fallbackPwd) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(pwd), []byte(am.fallbackPwd)) == 1
}

// checkLoginRate 登录限速：锁定中返回剩余时长
func (am *AuthManager) checkLoginRate(key string) time.Duration {
	am.failMu.Lock()
	defer am.failMu.Unlock()
	f, ok := am.failures[key]
	if !ok {
		return 0
	}
	if time.Now().Before(f.lockedUntil) {
		return time.Until(f.lockedUntil)
	}
	return 0
}

// recordLoginFail 记录失败并按指数退避锁定。
// 键由客户端可控输入构成且 /api/login 免认证：无界增长会被用来打爆内存，
// 超过容量上限时机会性清掉未锁定条目。
func (am *AuthManager) recordLoginFail(key string) {
	am.failMu.Lock()
	defer am.failMu.Unlock()
	if len(am.failures) >= 4096 {
		now := time.Now()
		for k, f := range am.failures {
			if now.After(f.lockedUntil) {
				delete(am.failures, k)
			}
		}
	}
	f := am.failures[key]
	if f == nil {
		f = &loginFail{}
		am.failures[key] = f
	}
	f.count++
	if f.count >= loginMaxFails {
		// 指数退避的位移必须封顶：60s<<28 会溢出 int64 变成负数，
		// 反而让锁定彻底失效（141 次失败后无限免费重试）
		k := f.count/loginMaxFails - 1
		if k > 8 {
			k = 8
		}
		backoff := loginLockBase << uint(k)
		if backoff > loginLockMax {
			backoff = loginLockMax
		}
		f.lockedUntil = time.Now().Add(backoff)
		log.Printf("[auth] login locked %s for %v (fails=%d)", key, backoff, f.count)
	}
}

func (am *AuthManager) clearLoginFail(key string) {
	am.failMu.Lock()
	delete(am.failures, key)
	am.failMu.Unlock()
}

func (am *AuthManager) adminUser() string {
	if am.db != nil {
		if u, err := am.db.GetAdminUser(); err == nil && u != "" {
			return u
		}
	}
	return "admin"
}

func (am *AuthManager) isDefaultPassword() bool {
	if am.db != nil {
		if isDefault, _ := am.db.IsDefaultPassword(); isDefault {
			return true
		}
		// 从未写过标记的老库：仍是无盐 admin 缺省哈希时继续提示
		if stored, _ := am.db.GetPasswordHash(); stored != "" && !strings.HasPrefix(stored, "$2") {
			return legacyHash("admin") == stored
		}
		return false
	}
	return am.fallbackPwd == "admin"
}

func generateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("[auth] crypto rand failed: %v", err)
	}
	return hex.EncodeToString(b)
}

// GenerateAPIKey 生成 sk- 前缀 API Key
func GenerateAPIKey() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("[auth] crypto rand failed: %v", err)
	}
	return "sk-" + hex.EncodeToString(b)
}

// Login 验证用户名密码，创建会话（带 IP+用户名 限速）
func (am *AuthManager) Login(username, password, clientIP string) (string, bool, time.Duration) {
	rateKey := clientIP + "|" + username
	if wait := am.checkLoginRate(rateKey); wait > 0 {
		return "", false, wait
	}

	// 口令验证（bcrypt 比较耗时数十至百毫秒）放在会话锁外，
	// 避免登录风暴期间阻塞所有持读锁的 /api 请求
	if username != am.adminUser() {
		am.recordLoginFail(rateKey)
		return "", false, 0
	}
	if !am.verifyPassword(password) {
		am.recordLoginFail(rateKey)
		return "", false, 0
	}

	am.mu.Lock()
	// 极小窗口：验证通过后、拿锁前口令被修改，会签出一个旧口令会话；
	// 下次登录即失效，可接受
	am.clearLoginFail(rateKey)

	now := time.Now()
	for token, s := range am.sessions {
		if now.After(s.expiresAt) {
			delete(am.sessions, token)
		}
	}
	token := generateToken()
	am.sessions[token] = &sessionEntry{username: username, createdAt: now, expiresAt: now.Add(sessionExpiry)}
	am.mu.Unlock()
	log.Printf("[auth] login success: user=%s", username)
	return token, true, 0
}

func (am *AuthManager) Logout(token string) {
	am.mu.Lock()
	defer am.mu.Unlock()
	delete(am.sessions, token)
}

func (am *AuthManager) IsValid(token string) bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	s, ok := am.sessions[token]
	if !ok {
		return false
	}
	return !time.Now().After(s.expiresAt)
}

// clientIP 提取客户端 IP（用于登录限速键）
// 服务仅绑定本机，X-Forwarded-For 可被任意伪造，一律以连接对端为准
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func extractToken(r *http.Request) string {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// ValidateAPIKey 校验 /v1 API Key
func (am *AuthManager) ValidateAPIKey(key string) bool {
	if am.db == nil || key == "" {
		return false
	}
	stored, err := am.db.GetAPIKey()
	if err != nil || stored == "" {
		return false
	}
	// 常数时间比较，规避计时侧信道
	return subtle.ConstantTimeCompare([]byte(key), []byte(stored)) == 1
}

// Middleware 认证中间件
func (am *AuthManager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// 免认证路径：登录接口、健康检查、Web 页面、OAuth 环回回调
		if path == "/api/login" || path == "/health" ||
			strings.HasPrefix(path, "/web") || strings.HasPrefix(path, "/oauth/") {
			next.ServeHTTP(w, r)
			return
		}

		// /v1/* 与 /async/*（闲时通道）使用 API Key 认证（Authorization: Bearer 或 x-api-key）。
		// 命中命名网关 Key（R1）时做启停/RPM 检查并注入 context，转发层再做
		// 模型白名单/配额拦截；根 Key（旧 api_key）不受限。
		if strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/async/") {
			var apiKey string
			if xKey := r.Header.Get("x-api-key"); xKey != "" {
				apiKey = xKey
			} else if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
				apiKey = strings.TrimPrefix(authHeader, "Bearer ")
			}
			if apiKey == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
					"error": map[string]string{
						"message": "API key required. Use 'Authorization: Bearer <key>' or 'x-api-key: <key>'",
						"type":    "authentication_error",
					},
				})
				return
			}
			gk, errResp := am.resolveGatewayKey(apiKey)
			if errResp != nil {
				if errResp.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "10")
				}
				writeJSON(w, errResp.status, map[string]interface{}{
					"error": map[string]string{"message": errResp.msg, "type": "authentication_error"},
				})
				return
			}
			if gk != nil {
				r = r.WithContext(contextWithGatewayKey(r.Context(), gk))
			}
			next.ServeHTTP(w, r)
			return
		}

		// /api/* 使用 session 认证（错误形状与其余 handler 统一为嵌套结构）
		if strings.HasPrefix(path, "/api/") {
			token := extractToken(r)
			if token == "" || !am.IsValid(token) {
				writeAPIError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// HandleLogin POST /api/login
func (am *AuthManager) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	token, ok, wait := am.Login(body.Username, body.Password, clientIP(r))
	if !ok {
		if wait > 0 {
			writeAPIError(w, http.StatusTooManyRequests,
				fmt.Sprintf("登录尝试过于频繁，请 %d 秒后再试", int(wait.Seconds())+1))
			return
		}
		writeAPIError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   int(sessionExpiry.Seconds()),
		SameSite: http.SameSiteStrictMode,
		// TLS 终止在反代（r.TLS == nil）时启发式失效：ZCODE_COOKIE_SECURE=1
		// 显式强制 Secure，防管理会话 cookie 经明文 http 请求外泄
		Secure: r.TLS != nil || cookieSecureOverride(),
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":             true,
		"token":               token,
		"username":            body.Username,
		"is_default_password": am.isDefaultPassword(),
	})
}

// HandleLogout POST /api/logout
func (am *AuthManager) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if token := extractToken(r); token != "" {
		am.Logout(token)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]string{"message": "logout success"})
}

// HandleCheckAuth GET /api/auth/check
func (am *AuthManager) HandleCheckAuth(w http.ResponseWriter, r *http.Request) {
	token := extractToken(r)
	if token != "" && am.IsValid(token) {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"authenticated":       true,
			"username":            am.adminUser(), // UI 顶栏显示真实管理员名（可自定义）
			"is_default_password": am.isDefaultPassword(),
		})
		return
	}
	writeAPIError(w, http.StatusUnauthorized, "not authenticated")
}

// HandleChangePassword POST /api/auth/password
func (am *AuthManager) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.NewPassword == "" {
		writeAPIError(w, http.StatusBadRequest, "new_password is required")
		return
	}
	if len(body.NewPassword) > maxPasswordBytes {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("new_password must be at most %d bytes", maxPasswordBytes))
		return
	}
	// 改密接口会拿旧口令做校验且成功即接管账号：与导出同样的 stolen-session
	// 威胁模型，必须有指数退避限速，否则可无节流爆破 old_password
	rateKey := clientIP(r) + "|password-change"
	if wait := am.checkLoginRate(rateKey); wait > 0 {
		writeAPIError(w, http.StatusTooManyRequests,
			fmt.Sprintf("尝试过于频繁，请 %d 秒后再试", int(wait.Seconds())+1))
		return
	}
	// 旧口令验证（bcrypt 耗时）放在会话锁外，与 Login 同一设计——
	// 否则改密风暴会以 ~100ms/次的速度冻结整个 /api 面
	if !am.verifyPassword(body.OldPassword) {
		am.recordLoginFail(rateKey)
		writeAPIError(w, http.StatusUnauthorized, "old password incorrect")
		return
	}
	am.clearLoginFail(rateKey)

	am.mu.Lock()
	defer am.mu.Unlock()

	if am.db != nil {
		hash, err := hashPassword(body.NewPassword)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := am.db.SetPasswordHash(hash); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "failed to save password")
			return
		}
		am.db.SetDefaultPasswordFlag(false)
	}
	am.sessions = make(map[string]*sessionEntry)
	log.Printf("[auth] password changed, all sessions invalidated")
	writeJSON(w, http.StatusOK, map[string]string{"message": "password changed, please re-login"})
}

// HandleGetAPIKey GET /api/settings/api-key
func (am *AuthManager) HandleGetAPIKey(w http.ResponseWriter, r *http.Request) {
	key, err := am.db.GetAPIKey()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"api_key": key, "has_api_key": key != ""})
}

// HandleGenerateAPIKey POST /api/settings/api-key/generate
func (am *AuthManager) HandleGenerateAPIKey(w http.ResponseWriter, r *http.Request) {
	newKey := GenerateAPIKey()
	if err := am.db.SetAPIKey(newKey); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("[auth] API key generated: %s...%s", newKey[:8], newKey[len(newKey)-4:])
	writeJSON(w, http.StatusOK, map[string]interface{}{"api_key": newKey, "message": "API key generated"})
}

// ---- HTTP 辅助 ----

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	// 管理接口会回显密钥/凭据/配置，禁止任何缓存层落盘
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{"message": msg, "type": "api_error"},
	})
}
