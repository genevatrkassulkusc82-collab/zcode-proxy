package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ---- Z.AI OAuth 授权码登录 ----
// 移植 zcode2api oauth.py：
//   一键登录(loopback): redirect_uri=http://127.0.0.1:{port}/oauth/callback, state=随机hex
//   手动粘贴(manual):   redirect_uri=https://zcode.z.ai/login, state=base64url(JSON{nonce,...})
// 兑换: POST zcode.z.ai/api/v1/oauth/token {"provider":"zai","code","redirect_uri","state"}
//   → data{token(Coding Plan JWT), zai{access_token,refresh_token}, user{...}}
// API Key 提取链: z/login → getCustomerInfo → api_keys(创建/复用 zcode-api-key) → copy → {key}.{secret}

const (
	OAuthAuthorizeURL   = "https://chat.z.ai/api/oauth/authorize"
	OAuthTokenURL       = "https://zcode.z.ai/api/v1/oauth/token"
	OAuthUserInfoURL    = "https://chat.z.ai/api/oauth/userinfo"
	OAuthClientID       = "client_P8X5CMWmlaRO9gyO-KSqtg"
	OAuthManualRedirect = "https://zcode.z.ai/login"
	BizLoginURL         = "https://api.z.ai/api/auth/z/login"
	CustomerInfoURL     = "https://api.z.ai/api/biz/customer/getCustomerInfo"
)

// OAuthFlow 一次登录流程的状态
type OAuthFlow struct {
	State        string `json:"state"`
	RedirectURI  string `json:"redirect_uri"`
	Manual       bool   `json:"manual"`
	Group        string `json:"group"`
	CreatedAt    int64  `json:"created_at"`
	Status       string `json:"status"` // pending | exchanging | ready | failed
	Message      string `json:"message"`
	AccountID    int64  `json:"account_id"`
	Email        string `json:"email"`
	AuthorizeURL string `json:"authorize_url"`
}

// OAuthManager OAuth 登录管理器
type OAuthManager struct {
	db   *DB
	zapi *ZCodeAPI
	port int // 本机监听端口（loopback redirect_uri 用）

	mu    sync.Mutex
	flows map[string]*OAuthFlow
}

// NewOAuthManager 创建 OAuth 管理器；listenAddr 形如 127.0.0.1:8687
func NewOAuthManager(db *DB, zapi *ZCodeAPI, listenAddr string) *OAuthManager {
	port := 8687
	if idx := strings.LastIndex(listenAddr, ":"); idx >= 0 {
		if p, err := strconv.Atoi(listenAddr[idx+1:]); err == nil {
			port = p
		}
	}
	return &OAuthManager{db: db, zapi: zapi, port: port, flows: make(map[string]*OAuthFlow)}
}

// StartLogin 创建登录流程，返回 (flow, 授权页 URL)
func (m *OAuthManager) StartLogin(manual bool, group string) (*OAuthFlow, string) {
	var state, redirectURI string
	if manual {
		nonce := uuid.NewString()
		redirectURI = OAuthManualRedirect
		stateJSON, _ := json.Marshal(map[string]string{
			"nonce":         nonce,
			"app_return_to": redirectURI,
			"redirect_uri":  redirectURI,
		})
		state = base64.RawURLEncoding.EncodeToString(stateJSON)
	} else {
		state = strings.ReplaceAll(uuid.NewString(), "-", "")[:32]
		redirectURI = fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", m.port)
	}
	authURL := OAuthAuthorizeURL + "?" + url.Values{
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"client_id":     {OAuthClientID},
		"state":         {state},
	}.Encode()

	flow := &OAuthFlow{
		State: state, RedirectURI: redirectURI, Manual: manual, Group: group,
		CreatedAt: time.Now().Unix(), Status: "pending", AuthorizeURL: authURL,
	}
	m.mu.Lock()
	m.flows[state] = flow
	// 清理 10 分钟前的过期流程
	for k, f := range m.flows {
		if time.Now().Unix()-f.CreatedAt > 600 {
			delete(m.flows, k)
		}
	}
	m.mu.Unlock()
	return flow, authURL
}

// beginExchange 单次消费闸门：原子地检查状态并置为 exchanging。
// ready/exchanging 状态拒绝——刷新结果页或重复提交会带着已被上游消费的
// code 再次兑换，把已成功的登录翻转成 failed
func (m *OAuthManager) beginExchange(f *OAuthFlow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch f.Status {
	case "ready":
		return errFlowDone
	case "exchanging":
		return errFlowBusy
	}
	f.Status = "exchanging"
	f.Message = "正在兑换 token…"
	return nil
}

var (
	errFlowDone = plainError("该登录流程已完成，请勿重复提交")
	errFlowBusy = plainError("该登录流程正在兑换中，请稍候")
)

// HandleCallback 浏览器环回回调：捕获 code → 后台兑换 → 返回结果页
func (m *OAuthManager) HandleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	code := strings.TrimSpace(q.Get("code"))
	errParam := strings.TrimSpace(q.Get("error"))
	errDesc := strings.TrimSpace(q.Get("error_description"))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	m.mu.Lock()
	flow, ok := m.flows[state]
	m.mu.Unlock()
	if !ok {
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding-top:80px">
			<h2>❌ 登录流程不存在或已过期</h2><p>请回到管理界面重新发起登录</p></body></html>`)
		return
	}
	if errParam != "" {
		// 原子守卫：状态检查与翻转在同一临界区内，error 回调不得翻转
		// 已成功（ready）或兑换中（exchanging）的流程
		m.mu.Lock()
		if flow.Status == "ready" || flow.Status == "exchanging" {
			m.mu.Unlock()
			fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding-top:80px">
				<h2>ℹ️ 登录流程已在进行中或已完成</h2><p>请回到管理界面查看登录状态。</p></body></html>`)
			return
		}
		flow.Status = "failed"
		flow.Message = fmt.Sprintf("授权被拒绝: %s", firstNonEmpty(errDesc, errParam))
		m.mu.Unlock()
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding-top:80px">
			<h2>❌ 授权被拒绝</h2><p>`+escapeHTML(firstNonEmpty(errDesc, errParam))+`</p></body></html>`)
		return
	}
	if code == "" {
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding-top:80px">
			<h2>❌ 回调缺少 code</h2></body></html>`)
		return
	}

	// 单次消费：重放（如刷新结果页）按既有状态如实回显，不重复兑换
	if err := m.beginExchange(flow); err != nil {
		m.mu.Lock()
		st, email := flow.Status, flow.Email
		m.mu.Unlock()
		if st == "ready" {
			fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding-top:80px">
				<h2>✅ 授权已完成</h2><p>账号 `+escapeHTML(email)+` 已入库，请回到管理界面查看。</p>
				<script>setTimeout(function(){window.close()},3000)</script></body></html>`)
			return
		}
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding-top:80px">
			<h2>⏳ `+escapeHTML(err.Error())+`</h2><p>请回到管理界面查看登录状态。</p></body></html>`)
		return
	}
	// 同步兑换：页面直接展示成功（含邮箱）或具体错误，避免"显示成功但后台静默失败"
	log.Printf("[oauth] callback arrived: state=%s… code_len=%d", safePrefixLog(state, 8), len(code))
	if err := m.completeFlow(flow, code); err != nil {
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding-top:80px">
			<h2>❌ 授权码兑换失败</h2><p>`+escapeHTML(err.Error())+`</p>
			<p style="color:#666;font-size:13px">可回到管理界面使用「OAuth 登录 → 手动粘贴」重试（把授权后地址栏完整 URL 贴入）。</p>
			</body></html>`)
		return
	}
	email := ""
	m.mu.Lock()
	email = flow.Email
	m.mu.Unlock()
	fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding-top:80px">
		<h2>✅ 授权成功</h2><p>账号 `+escapeHTML(email)+` 已入库，请回到管理界面查看。</p>
		<script>setTimeout(function(){window.close()},3000)</script></body></html>`)
}

// safePrefixLog 日志用安全前缀（不泄露完整 state）
func safePrefixLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// SubmitManual 手动粘贴模式：提交回跳 URL 或 code
func (m *OAuthManager) SubmitManual(state, raw string) error {
	m.mu.Lock()
	flow, ok := m.flows[state]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("登录流程不存在或已过期")
	}
	code := ExtractOAuthCode(raw)
	if code == "" {
		return fmt.Errorf("未能从输入中提取授权码")
	}
	// 单次消费：已完成/进行中的流程拒绝重复兑换
	if err := m.beginExchange(flow); err != nil {
		return err
	}
	log.Printf("[oauth] manual submit: state=%s… code_len=%d", safePrefixLog(state, 8), len(code))
	return m.completeFlow(flow, code)
}

// ExtractOAuthCode 容忍粘贴整个回跳 URL，从中提取 code
func ExtractOAuthCode(raw string) string {
	raw = strings.TrimSpace(strings.Trim(raw, `"'`))
	if strings.Contains(raw, "code=") {
		if u, err := url.Parse(raw); err == nil {
			return u.Query().Get("code")
		}
		return ""
	}
	return raw
}

// FlowStatus 查询流程状态（UI 轮询）
func (m *OAuthManager) FlowStatus(state string) *OAuthFlow {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.flows[state]
	if !ok {
		return nil
	}
	cp := *f
	return &cp
}

// setFlowStatus 更新流程状态与提示（poll 路径的中间态）
func (m *OAuthManager) setFlowStatus(f *OAuthFlow, status, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f.Status = status
	f.Message = msg
}

func (m *OAuthManager) finishFlow(f *OAuthFlow, email, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 已成功的流程不得被并发失败翻转（如 pollLoop 超时与兑换完成赛跑）
	if f.Status == "ready" {
		return
	}
	if errMsg != "" {
		f.Status = "failed"
		f.Message = errMsg
	} else {
		f.Status = "ready"
		f.Email = email
	}
}

// ---- 兑换流程 ----

// completeFlow code → token → API Key → 账号入库；返回错误供回调页展示
func (m *OAuthManager) completeFlow(flow *OAuthFlow, code string) error {
	data, err := m.exchangeToken(code, flow.State, flow.RedirectURI)
	if err != nil {
		m.finishFlow(flow, "", "token 兑换失败: "+err.Error())
		log.Printf("[oauth] exchange failed: %v", err)
		return fmt.Errorf("token 兑换失败: %w", err)
	}
	return m.ingestTokens(flow, data)
}

// ingestTokens 用已换取的 token 数据完成 API Key 提取与账号入库
// （授权码兑换与 cli/poll 轮询两条登录路径共用；data 形如
// {token(Coding Plan JWT), zai:{access_token, refresh_token}, user:{...}}）
func (m *OAuthManager) ingestTokens(flow *OAuthFlow, data map[string]interface{}) error {
	jwt := jsonStr(data, "token")
	if jwt == "" {
		m.finishFlow(flow, "", "返回数据中不含 Coding Plan JWT")
		return fmt.Errorf("返回数据中不含 Coding Plan JWT")
	}
	zai, _ := data["zai"].(map[string]interface{})
	accessToken := jsonStr(zai, "access_token")
	if accessToken == "" {
		accessToken = jsonStr(zai, "accessToken") // 官方 ready 载荷两种拼写都可能出现
	}
	refreshToken := jsonStr(zai, "refresh_token")
	user, _ := data["user"].(map[string]interface{})
	if user == nil {
		user = map[string]interface{}{}
	}

	// 用户信息缺失时补查 userinfo
	if jsonStr(user, "email") == "" && jsonStr(user, "user_id") == "" && accessToken != "" {
		if ui := m.fetchUserInfo(accessToken); ui != nil {
			user = ui
		}
	}

	userInfoJSON, _ := json.Marshal(user)
	email := jsonStr(user, "email")
	userID := firstNonEmpty(jsonStr(user, "user_id"), jsonStr(user, "id"))
	if userID == "" {
		if claims, err := DecodeJWTPayload(jwt); err == nil {
			userID = firstNonEmpty(jsonStr(claims, "user_id"), jsonStr(claims, "sub"))
		}
	}
	if userID == "" {
		userID = "oauth-" + uuid.NewString()[:8]
	}

	a := &Account{
		UserID:       userID,
		Email:        email,
		DisplayName:  oauthDisplayName(user),
		Provider:     "zai",
		AuthType:     "jwt",
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ZCodeJWT:     jwt,
		UserInfo:     string(userInfoJSON),
		DeviceMid:    LocalDeviceMid(),
		Status:       StatusActive,
		Enabled:      true,
		AccountGroup: flow.Group,
	}

	// API Key 提取链（best-effort：失败不影响 JWT 通道入库）
	if accessToken != "" {
		if apiKey, err := m.exchangeAPIKey(accessToken); err != nil {
			log.Printf("[oauth] api key extraction failed for %s: %v", email, err)
		} else {
			a.APIKey = apiKey
			log.Printf("[oauth] api key extracted for %s", email)
		}
	}

	id, err := m.db.UpsertAccount(a)
	if err != nil {
		m.finishFlow(flow, "", "账号入库失败: "+err.Error())
		return fmt.Errorf("账号入库失败: %w", err)
	}
	a.ID = id
	m.mu.Lock()
	flow.AccountID = id
	m.mu.Unlock()
	m.finishFlow(flow, email, "")
	log.Printf("[oauth] login success: %s (id=%d)", email, id)

	// 异步刷新额度确认套餐状态
	go func() {
		time.Sleep(time.Second)
		if err := m.zapi.RefreshAccountQuota(a); err != nil {
			log.Printf("[oauth] initial quota refresh %s: %v", email, err)
		}
	}()
	return nil
}

// exchangeToken POST /api/v1/oauth/token
func (m *OAuthManager) exchangeToken(code, state, redirectURI string) (map[string]interface{}, error) {
	payload, _ := json.Marshal(map[string]string{
		"provider":     "zai",
		"code":         code,
		"redirect_uri": redirectURI,
		"state":        state,
	})
	client := ClientForURL(m.zapi.egress.GlobalProxyURL(), OAuthTokenURL, 30*time.Second)
	resp, err := client.Post(OAuthTokenURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v map[string]interface{}
	json.Unmarshal(body, &v)
	// 状态码先于响应体判类（与 pollOnce 同判）：客户端不跟随重定向，3xx 一律
	// 按 WAF/挑战页处理——带 JSON 壳的挑战页否则会落进"不含 Coding Plan JWT"
	// 的误导提示，让用户对着一堵墙反复重试手动粘贴
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("HTTP %d: 疑似 WAF/挑战页重定向（%s），请稍后重试或更换出口代理", resp.StatusCode, truncate(strings.TrimSpace(string(body)), 120))
	}
	if len(v) == 0 && resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	// 业务码兼容 0/200/缺失
	if codeVal, has := v["code"]; has {
		n := jsonInt(v, "code")
		if n != 0 && n != 200 {
			msg := firstNonEmpty(jsonStr(v, "msg"), jsonStr(v, "message"))
			if msg == "" {
				msg = truncate(string(body), 200)
			}
			return nil, fmt.Errorf("业务码 %d: %s（HTTP %d）", n, msg, resp.StatusCode)
		}
		_ = codeVal
	}
	data, ok := v["data"].(map[string]interface{})
	if !ok || jsonStr(data, "token") == "" {
		return nil, fmt.Errorf("返回数据中不含 Coding Plan JWT")
	}
	return data, nil
}

// fetchUserInfo GET chat.z.ai/api/oauth/userinfo
func (m *OAuthManager) fetchUserInfo(accessToken string) map[string]interface{} {
	client := NewUpstreamHTTPClient(m.zapi.egress.GlobalProxyURL(), 15*time.Second)
	req, _ := http.NewRequest("GET", OAuthUserInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v map[string]interface{}
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	if d, ok := v["data"].(map[string]interface{}); ok {
		return d
	}
	return nil
}

// exchangeAPIKey OAuth access_token → 业务 token → 机构/项目 → API Key（oauth.py 移植）
func (m *OAuthManager) exchangeAPIKey(accessToken string) (string, error) {
	client := NewUpstreamHTTPClient(m.zapi.egress.GlobalProxyURL(), 30*time.Second)

	// 1. z/login 换业务 token
	loginBody, _ := json.Marshal(map[string]string{"token": accessToken})
	resp, err := client.Post(BizLoginURL, "application/json", bytes.NewReader(loginBody))
	if err != nil {
		return "", err
	}
	bizToken, err := readDataString(resp, "access_token", "accessToken")
	if err != nil {
		return "", fmt.Errorf("业务登录失败: %w", err)
	}

	// 2. getCustomerInfo 找默认机构/项目
	req, _ := http.NewRequest("GET", CustomerInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+bizToken)
	resp2, err := client.Do(req)
	if err != nil {
		return "", err
	}
	body2, _ := io.ReadAll(io.LimitReader(resp2.Body, 4<<20))
	resp2.Body.Close()
	var info struct {
		Data struct {
			Organizations []struct {
				OrganizationID   interface{} `json:"organizationId"`
				OrganizationName string      `json:"organizationName"`
				Projects         []struct {
					ProjectID   interface{} `json:"projectId"`
					ProjectName string      `json:"projectName"`
				} `json:"projects"`
			} `json:"organizations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body2, &info); err != nil {
		return "", fmt.Errorf("机构信息解析失败: %w", err)
	}
	orgs := info.Data.Organizations
	if len(orgs) == 0 {
		return "", fmt.Errorf("找不到可用的机构")
	}
	org := orgs[0]
	for _, o := range orgs {
		if strings.Contains(o.OrganizationName, "默认机构") {
			org = o
			break
		}
	}
	if len(org.Projects) == 0 {
		return "", fmt.Errorf("找不到可用的项目")
	}
	proj := org.Projects[0]
	for _, p := range org.Projects {
		if strings.Contains(p.ProjectName, "默认项目") {
			proj = p
			break
		}
	}
	orgID := fmt.Sprintf("%v", org.OrganizationID)
	projID := fmt.Sprintf("%v", proj.ProjectID)
	keyURL := fmt.Sprintf("https://api.z.ai/api/biz/v1/organization/%s/projects/%s/api_keys", orgID, projID)

	// 3. 列出 api_keys，找/建 zcode-api-key
	req3, _ := http.NewRequest("GET", keyURL, nil)
	req3.Header.Set("Authorization", "Bearer "+bizToken)
	resp3, err := client.Do(req3)
	if err != nil {
		return "", err
	}
	body3, _ := io.ReadAll(io.LimitReader(resp3.Body, 4<<20))
	resp3.Body.Close()
	var keysResp struct {
		Data []struct {
			Name   string `json:"name"`
			APIKey string `json:"apiKey"`
		} `json:"data"`
	}
	json.Unmarshal(body3, &keysResp)
	apiKey := ""
	for _, k := range keysResp.Data {
		if k.Name == "zcode-api-key" && k.APIKey != "" {
			apiKey = k.APIKey
			break
		}
	}
	if apiKey == "" {
		createBody, _ := json.Marshal(map[string]string{"name": "zcode-api-key"})
		req4, _ := http.NewRequest("POST", keyURL, bytes.NewReader(createBody))
		req4.Header.Set("Authorization", "Bearer "+bizToken)
		req4.Header.Set("Content-Type", "application/json")
		resp4, err := client.Do(req4)
		if err != nil {
			return "", err
		}
		body4, _ := io.ReadAll(io.LimitReader(resp4.Body, 1<<20))
		resp4.Body.Close()
		var created struct {
			Data struct {
				APIKey string `json:"apiKey"`
			} `json:"data"`
		}
		json.Unmarshal(body4, &created)
		apiKey = created.Data.APIKey
	}
	if apiKey == "" {
		return "", fmt.Errorf("获取 API Key 失败")
	}

	// 4. copy 接口取 secretKey
	req5, _ := http.NewRequest("GET", keyURL+"/copy/"+apiKey, nil)
	req5.Header.Set("Authorization", "Bearer "+bizToken)
	resp5, err := client.Do(req5)
	if err != nil {
		return "", err
	}
	secretKey, err := readDataString(resp5, "secretKey")
	if err != nil {
		return "", fmt.Errorf("未能解密 Secret Key: %w", err)
	}
	return apiKey + "." + secretKey, nil
}

// readDataString 从响应 data 对象中按候选键取字符串
func readDataString(resp *http.Response, keys ...string) (string, error) {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 150))
	}
	var v map[string]interface{}
	if json.Unmarshal(body, &v) != nil {
		return "", fmt.Errorf("响应非 JSON")
	}
	data, _ := v["data"].(map[string]interface{})
	if data == nil {
		return "", fmt.Errorf("响应缺少 data")
	}
	for _, k := range keys {
		if s := jsonStr(data, k); s != "" {
			return s, nil
		}
	}
	return "", fmt.Errorf("data 中缺少 %v", keys)
}

// oauthDisplayName 昵称 → 邮箱前缀 → 手机号（oauth.py display_name 移植）
func oauthDisplayName(user map[string]interface{}) string {
	pick := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := user[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	if name := pick("name", "username", "nickName", "nickname", "displayName"); name != "" {
		return name
	}
	if email := pick("email", "mail"); strings.Contains(email, "@") {
		return strings.SplitN(email, "@", 2)[0]
	}
	if phone := pick("phone", "phone_number", "phoneNumber", "mobile"); phone != "" {
		return "账号" + phone
	}
	return ""
}

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
