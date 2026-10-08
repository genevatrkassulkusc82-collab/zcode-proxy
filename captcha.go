package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

// ---- 阿里云无痕验证码求解服务 ----
// 移植 zcode2api captcha.py（Playwright → go-rod）：
//   1. GET client/configs 拿 captcha 配置（prefix/region/sceneId，缓存 10 分钟）
//   2. 本机真实 Chrome/Edge（捆绑 Chromium 会被风控识别）打开 zcode.z.ai 同源页
//   3. 注入阿里云无痕验证 SDK HTML，自动触发 startTracelessVerification
//   4. window.__onCaptcha 回调捕获 success param（即 X-Aliyun-Captcha-Verify-Param）
//   5. 参数按出口代理分组缓存 45s；过期后 300s 宽限期内返回旧参数并后台刷新
//   6. 无头失败自动升级有头窗口让用户手动过，结果同样入缓存
// 并发模型：按出口代理分组的容量 1 信号量（组间互不阻塞），前台求解有界等待 45s、
// 超时返回繁忙错误，后台刷新非阻塞 TryAcquire；浏览器操作全程有 context 上界，
// 代理黑洞/页面卡死只会占用信号量到上限，不会永久占坑。

const (
	captchaCacheTTL       = 45 * time.Second
	captchaStaleGrace     = 300 * time.Second
	captchaFailCacheTTL   = 60 * time.Second
	captchaConfigTTL      = 10 * time.Minute
	captchaAcquireTimeout = 45 * time.Second
	captchaSolveTimeout   = 40 * time.Second
	captchaLaunchTimeout  = 30 * time.Second
	captchaSolveRetries   = 4
	captchaChromeUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// CaptchaConfig 验证码配置（client/configs 响应）
type CaptchaConfig struct {
	Enabled bool   `json:"enabled"`
	Region  string `json:"region"`
	Prefix  string `json:"prefix"`
	SceneID string `json:"scene_id"`
}

type captchaCacheEntry struct {
	param  string
	region string
	at     time.Time
}

// CaptchaService 验证码求解服务（并发安全）
type CaptchaService struct {
	cfg        *FileConfig
	db         *DB
	appVersion string

	mu       sync.Mutex
	cache    map[string]*captchaCacheEntry // key = 出口代理 URL（多代理组隔离）
	failAt   map[string]time.Time
	config   *CaptchaConfig
	configAt time.Time

	semMu  sync.Mutex
	sems   map[string]chan struct{} // key = 出口代理组（容量 1 信号量，组间互不阻塞）
	manual bool                     // 有头手动模式（自动失败后升级）
}

// NewCaptchaService 创建验证码服务
func NewCaptchaService(cfg *FileConfig, db *DB, appVersion string) *CaptchaService {
	return &CaptchaService{
		cfg:        cfg,
		db:         db,
		appVersion: appVersion,
		cache:      make(map[string]*captchaCacheEntry),
		failAt:     make(map[string]time.Time),
		sems:       make(map[string]chan struct{}),
	}
}

func (s *CaptchaService) cacheKey(a *Account) string {
	return captchaProxyHook(a)
}

// captchaHTML 阿里云无痕验证注入页（与 zcode2api 逐字一致；配置值经 JSON 转义防注入）
func captchaHTML(sceneID, region, prefix string) string {
	js := func(v string) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return `<!DOCTYPE html><html><head><meta charset="utf-8">
<script src="https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"></script>
</head><body><div id="cap"></div><button id="btn"></button>
<script>
window.initAliyunCaptcha({
  SceneId: ` + js(sceneID) + `, mode: 'popup', region: ` + js(region) + `, prefix: ` + js(prefix) + `,
  element: '#cap', button: '#btn', captchaLogoImg: '', showErrorTip: false,
  getInstance: function (inst) {
    var fn = inst.startTracelessVerification || inst.show;
    try { fn.call(inst); } catch (e) {
      window.__onCaptcha(JSON.stringify({event: 'starterr', message: String(e && e.message || e)}));
    }
  },
  success: function (param) { window.__onCaptcha(JSON.stringify({event: 'success', param: param})); },
  fail: function (m) { window.__onCaptcha(JSON.stringify({event: 'fail', reason: m})); },
  onError: function (m) { window.__onCaptcha(JSON.stringify({event: 'error', reason: m})); }
});
</script></body></html>`
}

// GetVerifyParam 获取有效验证参数（缓存 → 宽限期旧值 → 重新求解）
func (s *CaptchaService) GetVerifyParam(a *Account) (param, region string, err error) {
	mode := s.getSetting("captcha_mode")
	if mode == "off" {
		return "", "", nil // 关闭验证码：直连（上游可能已放宽）
	}
	key := s.cacheKey(a)

	s.mu.Lock()
	if e, ok := s.cache[key]; ok {
		age := time.Since(e.at)
		if age < captchaCacheTTL {
			p, r := e.param, e.region
			s.mu.Unlock()
			return p, r, nil
		}
		if age < captchaCacheTTL+captchaStaleGrace {
			p, r := e.param, e.region
			s.mu.Unlock()
			go s.refreshInBackground(a) // 宽限期：旧值先用，后台刷新
			return p, r, nil
		}
	}
	if t, ok := s.failAt[key]; ok && time.Since(t) < captchaFailCacheTTL {
		s.mu.Unlock()
		return "", "", fmt.Errorf("验证码求解近期失败（风控冷却中），请稍后重试")
	}
	s.mu.Unlock()

	return s.solveOnce(a)
}

func (s *CaptchaService) getSetting(key string) string {
	if s.db == nil {
		return ""
	}
	v, _ := s.db.GetSetting(key)
	return v
}

// groupSem 返回出口代理组（代理 URL，空则 default）对应的容量 1 求解信号量
func (s *CaptchaService) groupSem(a *Account) chan struct{} {
	group := s.cacheKey(a)
	if group == "" {
		group = "default"
	}
	s.semMu.Lock()
	defer s.semMu.Unlock()
	if ch, ok := s.sems[group]; ok {
		return ch
	}
	ch := make(chan struct{}, 1)
	s.sems[group] = ch
	return ch
}

// tryAcquireSolve 非阻塞获取求解权（后台刷新用；拿不到说明该组已有求解在跑）
func (s *CaptchaService) tryAcquireSolve(sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// acquireSolve 有界等待获取求解权：最长等 captchaAcquireTimeout，超时返回 false（前台同步路径用）
func (s *CaptchaService) acquireSolve(sem chan struct{}) bool {
	timer := time.NewTimer(captchaAcquireTimeout)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

func (s *CaptchaService) releaseSolve(sem chan struct{}) { <-sem }

func (s *CaptchaService) refreshInBackground(a *Account) {
	sem := s.groupSem(a)
	if !s.tryAcquireSolve(sem) {
		return
	}
	defer s.releaseSolve(sem)
	s.doSolve(a)
}

func (s *CaptchaService) solveOnce(a *Account) (string, string, error) {
	sem := s.groupSem(a)
	if !s.acquireSolve(sem) {
		return "", "", fmt.Errorf("验证码求解繁忙（等待 %v 超时），请稍后重试", captchaAcquireTimeout)
	}
	defer s.releaseSolve(sem)

	key := s.cacheKey(a)
	// 双检：等信号量期间可能已被其他请求求解成功
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && time.Since(e.at) < captchaCacheTTL {
		p, r := e.param, e.region
		s.mu.Unlock()
		return p, r, nil
	}
	s.mu.Unlock()
	return s.doSolve(a)
}

func (s *CaptchaService) doSolve(a *Account) (string, string, error) {
	key := s.cacheKey(a)
	cc, err := s.fetchConfig(a)
	if err != nil {
		s.markFail(key)
		return "", "", err
	}
	if !cc.Enabled {
		return "", "", nil // 上游未开启验证码
	}

	mode := s.getSetting("captcha_mode")
	s.mu.Lock()
	manual := s.manual
	s.mu.Unlock()
	headless := mode != "manual" && !manual

	var lastErr error
	for attempt := 1; attempt <= captchaSolveRetries; attempt++ {
		param, err := s.solveWithBrowser(cc, headless, a)
		if err == nil && param != "" {
			s.mu.Lock()
			s.cache[key] = &captchaCacheEntry{param: param, region: cc.Region, at: time.Now()}
			delete(s.failAt, key)
			s.manual = false // 成功后回到无头模式
			s.mu.Unlock()
			log.Printf("[captcha] solved (headless=%v, attempt=%d, len=%d)", headless, attempt, len(param))
			return param, cc.Region, nil
		}
		lastErr = err
		log.Printf("[captcha] solve attempt %d failed (headless=%v): %v", attempt, headless, err)
		// 无头连续失败 2 次后升级有头手动模式
		if headless && attempt >= 2 {
			headless = false
			s.mu.Lock()
			s.manual = true
			s.mu.Unlock()
			log.Printf("[captcha] switching to headed manual mode")
		}
	}
	s.markFail(key)
	return "", "", fmt.Errorf("验证码求解失败（%d 次尝试）: %v", captchaSolveRetries, lastErr)
}

func (s *CaptchaService) markFail(key string) {
	s.mu.Lock()
	s.failAt[key] = time.Now()
	s.mu.Unlock()
}

// InvalidateFor 上游拒绝时失效该出口代理的缓存
func (s *CaptchaService) InvalidateFor(a *Account) {
	s.mu.Lock()
	delete(s.cache, s.cacheKey(a))
	s.mu.Unlock()
}

// prewarmTick prewarm 周期（节流上限：每 15s 最多触发一轮后台刷新）
const prewarmTick = 15 * time.Second

// prewarmFreshAhead 提前刷新线：缓存条目年龄超过该值即在后台换新，
// 使转发请求在 45s TTL 内永远命中新鲜参数（请求路径零求解延迟）
const prewarmRefreshAhead = 30 * time.Second

// StartPrewarm 参数保温循环（速度优先）：常驻后台按出口代理组把验证参数
// 保持在新鲜状态，转发与自动领取的 GetVerifyParam 全部命中缓存。
// captcha_prewarm 设为 "0" 可关闭（如需完全静默降低求解频率）。
func (s *CaptchaService) StartPrewarm(stopCh <-chan struct{}) {
	go func() {
		// 首轮延迟 10s：等服务起来、账号导入完成
		t := time.NewTimer(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
			}
			s.prewarmOnce()
			t.Reset(prewarmTick)
		}
	}()
}

// prewarmOnce 单轮保温：找出可选的 zai JWT 账号，按出口代理组去重后，
// 对新鲜度不足的组触发后台求解（组信号量保证与前台求解互斥、重复轮次合并）
func (s *CaptchaService) prewarmOnce() {
	if s.getSetting("captcha_mode") == "off" {
		return
	}
	if v, _ := s.db.GetSetting("captcha_prewarm"); v == "0" {
		return
	}
	accounts, err := s.db.ListAccounts("")
	if err != nil {
		return
	}
	now := time.Now()
	seen := map[string]bool{}
	s.mu.Lock()
	var stale []string
	for _, a := range accounts {
		if !a.Enabled || a.Provider != "zai" || a.ZCodeJWT == "" {
			continue
		}
		if !accountSelectable(a, now.Unix()) {
			continue
		}
		key := s.cacheKey(a)
		if seen[key] {
			continue
		}
		seen[key] = true
		if e, ok := s.cache[key]; ok && now.Sub(e.at) < prewarmRefreshAhead {
			continue // 仍然新鲜
		}
		if t, ok := s.failAt[key]; ok && now.Sub(t) < captchaFailCacheTTL {
			continue // 近期求解失败，等待冷却，不硬顶
		}
		stale = append(stale, key)
	}
	s.mu.Unlock()

	// 刷新动作代理到组内任一账号：参数按出口代理组缓存，组内账号等价
	byKey := map[string]*Account{}
	for _, a := range accounts {
		if !a.Enabled || a.Provider != "zai" || a.ZCodeJWT == "" {
			continue
		}
		byKey[s.cacheKey(a)] = a
	}
	for _, key := range stale {
		a := byKey[key]
		if a == nil {
			continue
		}
		log.Printf("[captcha] prewarm: refreshing param for egress group")
		s.refreshInBackground(a)
	}
}

// Invalidate 失效全部缓存
func (s *CaptchaService) Invalidate() {
	s.mu.Lock()
	s.cache = make(map[string]*captchaCacheEntry)
	s.mu.Unlock()
}

// Status 服务状态（UI 展示）
func (s *CaptchaService) Status() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	has, fresh, oldest := false, false, time.Duration(0)
	for _, e := range s.cache {
		has = true
		age := time.Since(e.at)
		if age < captchaCacheTTL {
			fresh = true
		}
		if age > oldest {
			oldest = age
		}
	}
	return map[string]interface{}{
		"has_param":   has,
		"param_age_s": int(oldest.Seconds()),
		"fresh":       fresh,
		"manual_mode": s.manual,
		"cache_keys":  len(s.cache),
		"config":      s.config,
	}
}

// fetchConfig 获取并缓存验证码配置
func (s *CaptchaService) fetchConfig(a *Account) (*CaptchaConfig, error) {
	s.mu.Lock()
	if s.config != nil && time.Since(s.configAt) < captchaConfigTTL {
		c := s.config
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()

	urlStr := fmt.Sprintf("%s?version=%s&os=%s", ClientConfigsURL, s.appVersion, NodePlatform())
	// 配置接口无需认证，但必须走全局出口代理（与上游其余调用同一网络路径），
	// 否则配置直连失败会让所有求解在起点就报废
	client := ClientForURL(captchaGlobalProxyHook(), urlStr, 20*time.Second)
	req, _ := http.NewRequest("GET", urlStr, nil)
	id := NewClientIdentity(s.appVersion, "")
	for k, v := range ZaiClientHeaders(id) {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取验证码配置失败: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Configs struct {
				Captcha struct {
					Enabled bool   `json:"enabled"`
					Prefix  string `json:"prefix"`
					Region  string `json:"region"`
					SceneID string `json:"sceneId"`
				} `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("验证码配置解析失败: %w", err)
	}
	if body.Code != 0 {
		return nil, fmt.Errorf("验证码配置接口业务码 %d: %s", body.Code, body.Msg)
	}
	c := body.Data.Configs.Captcha
	cc := &CaptchaConfig{Enabled: c.Enabled, Region: c.Region, Prefix: c.Prefix, SceneID: c.SceneID}

	s.mu.Lock()
	s.config = cc
	s.configAt = time.Now()
	s.mu.Unlock()
	log.Printf("[captcha] config: enabled=%v region=%s prefix=%s scene=%s", cc.Enabled, cc.Region, cc.Prefix, cc.SceneID)
	return cc, nil
}

// ---- rod 浏览器求解 ----

// findRealBrowser 定位本机真实 Chrome/Edge（捆绑 Chromium 会被阿里云风控识别）
func findRealBrowser() string {
	if runtime.GOOS != "windows" {
		for _, p := range []string{
			"/usr/bin/google-chrome", "/usr/bin/chromium-browser",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return ""
	}
	localAppData := os.Getenv("LOCALAPPDATA")
	programFiles := os.Getenv("ProgramFiles")
	programFilesX86 := os.Getenv("ProgramFiles(x86)")
	candidates := []string{
		filepath.Join(programFiles, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(programFilesX86, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(localAppData, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(programFilesX86, `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(programFiles, `Microsoft\Edge\Application\msedge.exe`),
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// solveWithBrowser 启动浏览器求解一次。
// 任何失败路径都保证已 Launch 的浏览器进程被回收（避免 profile 锁级联瘫痪）。
func (s *CaptchaService) solveWithBrowser(cc *CaptchaConfig, headless bool, a *Account) (param string, err error) {
	bin := findRealBrowser()
	l := launcher.New().
		Headless(headless).
		Set("no-sandbox").
		Set("disable-dev-shm-usage").
		Set("disable-blink-features", "AutomationControlled").
		Set("lang", "zh-CN").
		Set("user-agent", captchaChromeUA)
	if bin != "" {
		l = l.Bin(bin)
	} else {
		log.Printf("[captcha] real Chrome/Edge not found, using rod managed browser (may be flagged)")
	}
	// 持久化浏览器配置：保留阿里云风控 cookie，避免每次求解都被视为新设备。
	// 每个代理组独立 profile：Chrome 按 user-data-dir 强制进程单例，
	// 共用目录会让组间并行求解的第二次 Launch 静默失败
	if profileDir := browserProfileDirForGroup(s.cacheKey(a)); profileDir != "" {
		l = l.UserDataDir(profileDir)
	}
	// 走账号组出口代理（与上游请求同 IP，避免风控不一致）
	if proxyURL := captchaProxyHook(a); proxyURL != "" {
		l = l.Proxy(proxyURL)
	}

	// Launch 有界：Chrome 起不来/卡死时不能无限期占住组信号量
	type launchResult struct {
		url string
		err error
	}
	lch := make(chan launchResult, 1)
	go func() {
		u, err := l.Launch()
		lch <- launchResult{u, err}
	}()
	var controlURL string
	select {
	case res := <-lch:
		if res.err != nil {
			return "", fmt.Errorf("启动浏览器失败: %w", res.err)
		}
		controlURL = res.url
	case <-time.After(captchaLaunchTimeout):
		// 卡死的 Launch：即时杀一次，并留观察者在迟到的 Launch 完成后补杀——
		// 否则残留 Chrome 会一直占着该组的 user-data-dir 单例锁。
		// 观察者无界等待是刻意的：泊住一个 goroutine 远比泄漏一个
		// 占着 profile 锁的 Chrome 进程便宜（Launch 永不返回时泄漏的
		// 只有 launch goroutine 本身）
		l.Kill()
		go func() {
			if res := <-lch; res.err == nil {
				l.Kill()
			}
		}()
		return "", fmt.Errorf("启动浏览器超时（%v）", captchaLaunchTimeout)
	}
	// Launch 成功后立即登记兜底回收：Connect/后续任何失败都杀进程
	killed := false
	defer func() {
		if !killed {
			l.Kill()
		}
	}()

	// 浏览器全链路（Connect/Page/Expose/WaitLoad/Close）绑定有界 context：
	// 任一环节卡死只占用信号量到上限，随后由 Kill 兜底回收进程
	bctx, bcancel := context.WithTimeout(context.Background(), captchaSolveTimeout+captchaLaunchTimeout)
	defer bcancel()
	browser := rod.New().ControlURL(controlURL).Context(bctx)
	if err = browser.Connect(); err != nil {
		return "", fmt.Errorf("连接浏览器失败: %w", err)
	}
	// Connect 成功：交由 browser.Close 回收（含进程）；Close 失败（如超时）
	// 时 Kill 兜底，对已死进程幂等
	defer func() {
		killed = true
		if err := browser.Close(); err != nil {
			log.Printf("[captcha] browser close: %v", err)
			l.Kill()
		}
	}()

	page, err := browser.Page(proto.TargetCreateTarget{URL: "https://zcode.z.ai/"})
	if err != nil {
		return "", fmt.Errorf("打开页面失败: %w", err)
	}
	defer page.Close()
	if err = page.WaitLoad(); err != nil {
		log.Printf("[captcha] wait load: %v", err)
	}

	// 暴露回调：JS window.__onCaptcha(jsonString) → Go channel
	events := make(chan map[string]interface{}, 8)
	if _, err = page.Expose("__onCaptcha", func(j gson.JSON) (interface{}, error) {
		payload := j.Str()
		var m map[string]interface{}
		if json.Unmarshal([]byte(payload), &m) == nil {
			select {
			case events <- m:
			default:
			}
		}
		return nil, nil
	}); err != nil {
		return "", fmt.Errorf("暴露回调失败: %w", err)
	}

	// 注入验证码页
	html := captchaHTML(cc.SceneID, cc.Region, cc.Prefix)
	if err = page.SetDocumentContent(html); err != nil {
		return "", fmt.Errorf("注入页面失败: %w", err)
	}

	deadline := time.After(captchaSolveTimeout)
	for {
		select {
		case ev := <-events:
			name, _ := ev["event"].(string)
			switch name {
			case "success":
				if p, ok := ev["param"].(string); ok && p != "" {
					return p, nil
				}
				return "", fmt.Errorf("success 事件缺少 param")
			case "fail":
				reason, _ := ev["reason"].(string)
				return "", fmt.Errorf("验证失败: %s", reason)
			case "error":
				reason, _ := ev["reason"].(string)
				return "", fmt.Errorf("SDK 错误: %s", reason)
			case "starterr":
				msg, _ := ev["message"].(string)
				return "", fmt.Errorf("启动异常: %s", msg)
			}
		case <-deadline:
			return "", fmt.Errorf("求解超时（%v）", captchaSolveTimeout)
		}
	}
}

// captchaProxyHook 由 main 注入：返回账号组出口代理 URL
var captchaProxyHook = func(a *Account) string { return "" }

// captchaGlobalProxyHook 由 main 注入：返回全局出口代理 URL（config 接口用）
var captchaGlobalProxyHook = func() string { return "" }

// browserProfileHook 由 main 注入：返回持久化浏览器配置目录
var browserProfileHook = func() string { return "" }

// browserProfileDirForGroup 每个代理组独立 profile 子目录（组 key 哈希命名）
func browserProfileDirForGroup(group string) string {
	base := browserProfileHook()
	if base == "" {
		return ""
	}
	if group == "" {
		group = "default"
	}
	sum := sha256.Sum256([]byte(group))
	dir := filepath.Join(base, hex.EncodeToString(sum[:8]))
	os.MkdirAll(dir, 0755)
	return dir
}
