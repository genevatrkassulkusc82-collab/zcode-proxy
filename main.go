package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func main() {
	configDir := flag.String("config", "config", "config directory path")
	dbPath := flag.String("db", "data/zcode.db", "SQLite database path")
	doctor := flag.Bool("doctor", false, "run offline health checks and exit")
	flag.Parse()

	absDir, err := filepath.Abs(*configDir)
	if err != nil {
		log.Fatalf("resolve config path: %v", err)
	}
	os.MkdirAll(absDir, 0755)

	cfg, err := LoadFileConfig(absDir)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	log.Printf("[main] config dir: %s, models=%d", absDir, len(cfg.GetModels()))

	absDBPath, err := filepath.Abs(*dbPath)
	if err != nil {
		log.Fatalf("resolve db path: %v", err)
	}
	os.MkdirAll(filepath.Dir(absDBPath), 0755)
	// -doctor：离线体检（配置/库完整性/vault/账号/代理/Key/记录表行数），
	// 走无迁移打开——不建表、不迁移、不播种、不生成/轮换 vault 钥匙，
	// 体检备份库不会顺手把它升级；正常启动才执行全部写库初始化
	db, err := NewDBWithOptions(absDBPath, !*doctor)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	log.Printf("[main] database: %s", absDBPath)

	if *doctor {
		runDoctorAndExit(cfg, db)
	}

	stopHotReload := cfg.StartHotReload(30 * time.Second)
	defer stopHotReload()

	// 客户端伪装版本号：config 优先，其次注册表探测，最后内置默认
	appVersion := cfg.GetAppVersion()
	if appVersion == "" {
		appVersion = DetectZCodeAppVersion()
	}
	log.Printf("[main] zcode app version: %s", appVersion)

	// 账号池（状态机 + 选择策略 + 额度刷新循环）
	pool := NewAccountPool(db, cfg, appVersion)
	pool.Start()
	defer pool.Stop()

	// R5：无 device_mid 的账号（OAuth/粘贴导入）补齐独立设备指纹
	if n := EnsurePoolDeviceIdentities(db); n > 0 {
		log.Printf("[main] backfilled device identity for %d account(s)", n)
	}

	// TLS 指纹钩子（utls 预设 / 自定义 JA3）
	fingerprintHook = func() TLSFingerprint {
		mode, _ := db.GetSetting("fingerprint")
		ja3, _ := db.GetSetting("custom_ja3")
		if mode == "" {
			mode = "chrome"
		}
		return TLSFingerprint{Mode: mode, JA3: ja3}
	}
	// 迁移：旧库可能残留不在预置表中的指纹值（如 chrome_120），回落到 chrome
	if cur, _ := db.GetSetting("fingerprint"); cur != "" && !isValidFingerprint(cur) {
		db.SetSetting("fingerprint", "chrome")
		log.Printf("[main] migrated invalid fingerprint setting %q -> chrome", cur)
	}

	// 验证码求解服务（阿里云无痕验证，rod 驱动本机 Chrome/Edge）
	captcha := NewCaptchaService(cfg, db, appVersion)
	egress := NewEgressProxy(db)
	captchaProxyHook = func(a *Account) string { return egress.ProxyURLForAccount(a) }
	captchaGlobalProxyHook = func() string { return egress.GlobalProxyURL() }
	routingGlobalProxyHook = func() string { return egress.GlobalProxyURL() }
	browserProfileHook = func() string {
		exe, _ := os.Executable()
		return filepath.Join(filepath.Dir(exe), "data", "browser-profile")
	}

	// 上游 API 客户端封装（额度/活动/激活/聊天转发）
	zapi := NewZCodeAPI(cfg, db, pool, captcha, appVersion)

	// 验证码参数保温：后台持续换新缓存参数，转发请求零求解等待
	prewarmStop := make(chan struct{})
	defer close(prewarmStop)
	captcha.StartPrewarm(prewarmStop)

	// OAuth 登录管理（环回回调 + 手动粘贴兜底）
	oauth := NewOAuthManager(db, zapi, cfg.GetListenAddr())

	// 账号管理（本地客户端导入 / 粘贴导入 / 一键切回）
	acctMgr := NewAccountManager(db, zapi, oauth)

	// 活动计划调度器
	scheduler := NewCronScheduler(db, zapi)
	scheduler.Start()
	defer scheduler.Stop()

	// 自动领取促销活动（只领活动，绝不自动消耗重置）
	autoClaim := NewAutoClaimer(db, zapi)
	autoClaim.Start()
	defer autoClaim.Stop()

	// 记录保留清扫（默认 90 天，usage_retention_days 可调；0=永久）：
	// usage_records 每请求一行、claim_records 自动领取每账号每轮一行、
	// plan_run_records 分钟级计划一天 1440 行——三表共用一个旋钮，
	// 无界增长会让 stats 聚合随表龄线性变慢。
	// 停机顺序必须是"先关信号再 join"：单个 defer 内先 close(retentionStop)
	// 再等 retentionDone——LIFO 下若拆成两个 defer，join 会先于 close 执行，
	// 白等 5 秒且 join 永远落空
	retentionStop := make(chan struct{})
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		pruneOnce := func() {
			days := db.UsageRetentionDays()
			if days <= 0 {
				return
			}
			if n, err := db.PruneRecords(days); err != nil {
				log.Printf("[retention] record prune: %v", err)
			} else if n > 0 {
				log.Printf("[retention] pruned %d record(s) older than %dd (usage/claim/plan_run)", n, days)
			}
		}
		// 首次清扫延迟 10 分钟：老库首删可能锁住唯一连接数秒，避开启动窗口
		select {
		case <-retentionStop:
			return
		case <-time.After(10 * time.Minute):
		}
		pruneOnce()
		for {
			select {
			case <-retentionStop:
				return
			case <-t.C:
				pruneOnce()
			}
		}
	}()
	defer func() {
		close(retentionStop)
		select {
		case <-retentionDone:
		case <-time.After(5 * time.Second):
			log.Printf("[main] shutdown: retention goroutine did not stop in 5s")
		}
	}()

	// Web 认证
	auth := NewAuthManager(db, os.Getenv("ZCODE_WEB_PASS"))

	// 管理 REST API
	apiServer := NewAPIServer(db, cfg, pool, zapi, oauth, acctMgr, scheduler, auth, captcha)

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)

	// 2API 端点
	mux.HandleFunc("/v1/messages", zapi.HandleMessages)
	mux.HandleFunc("/v1/messages/", zapi.HandleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", zapi.HandleCountTokens)
	mux.HandleFunc("/v1/chat/completions", zapi.HandleChatCompletions)
	mux.HandleFunc("/v1/completions", zapi.HandleCompletions)
	mux.HandleFunc("/v1/responses", zapi.HandleResponses)
	mux.HandleFunc("/v1/models", zapi.HandleModels)
	mux.HandleFunc("/v1/models/", zapi.HandleModelRetrieve)

	// 闲时免费通道（off-peak ticket queue；设置 async_enabled 开启）
	mux.HandleFunc("/async/v1/messages", zapi.HandleAsyncMessages)

	// OAuth 环回回调（浏览器授权后跳转，无需认证）
	mux.HandleFunc("/oauth/callback", oauth.HandleCallback)

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "zcode-proxy"})
	})

	// 前端 Web 界面
	webContent, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		log.Fatalf("read embedded web/index.html: %v", err)
	}
	mux.HandleFunc("/web", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webContent)
	})
	webSub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("sub web fs: %v", err)
	}
	staticHandler := http.StripPrefix("/web/", http.FileServer(http.FS(webSub)))
	mux.HandleFunc("/web/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/web/static/") {
			// 静态资源强制 revalidate，避免升级后浏览器用旧 app.js/css 渲染出空控件
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
			staticHandler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webContent)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/web", http.StatusFound)
			return
		}
		// 未知 /api/*、/v1/* 返回 404，避免被兜底 200 吞掉（曾导致假登录/误判）
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") {
			writeAPIError(w, http.StatusNotFound, "not found: "+r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"service":"zcode-proxy","version":"1.0","endpoints":["/v1/messages","/v1/chat/completions","/v1/completions","/v1/responses","/v1/models","/api/","/web","/health"]}`)
	})

	listenAddr := cfg.GetListenAddr()
	// 非环回监听 + cookie 未强制 Secure：面板登录跨网明文传输，一条告警
	// 指路反代 + ZCODE_COOKIE_SECURE=1（auth.go 的 cookieSecureOverride）
	if !listenAddrIsLoopback(listenAddr) && !cookieSecureOverride() {
		log.Printf("[main] WARNING: listening on non-loopback address %s without forced-secure session cookies — "+
			"panel login crosses the network in cleartext; put a TLS reverse proxy in front and set ZCODE_COOKIE_SECURE=1", listenAddr)
	}
	log.Printf("[main] zcode-proxy listening on http://%s", listenAddr)
	log.Printf("[main] web UI: http://%s/web", listenAddr)
	// 显式 Server：ReadHeaderTimeout 防 Slowloris；SSE 决定不设 WriteTimeout。
	// 在途请求计数：停机时先等处理器退出，再走 deferred 池停止与 db.Close()，
	// 否则长 SSE 期间 usage/状态写库会撞上已关闭的库
	var inFlight sync.WaitGroup
	authed := limitBody(auth.Middleware(mux))
	// panic 隔离：net/http 虽自带 recover（进程不死），但客户端只看到连接重置、
	// 无状态码无错误体。响应未开始时补一个 502；已开流的只记日志（协议帧格式
	// 依 proto 而异，中间层无法可靠代写）
	srv := &http.Server{
		Addr: listenAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inFlight.Add(1)
			defer inFlight.Done()
			wt := &writeTracker{ResponseWriter: w}
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[http] handler panic on %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
					if !wt.wrote {
						wt.Header().Set("Content-Type", "application/json")
						wt.WriteHeader(http.StatusBadGateway)
						fmt.Fprint(wt, `{"error":{"message":"internal error (panic contained)","type":"api_error"}}`)
					}
				}
			}()
			authed.ServeHTTP(wt, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 优雅停机：等待 SIGINT/SIGTERM，给在途请求（含 SSE）一个有界排水窗口
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("[main] shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// 排水超时（长 SSE 是常态）：强制断连，处理器在下次写响应时退出
		log.Printf("[main] shutdown: %v; forcing close of active connections", err)
		srv.Close()
	}
	// 给在途处理器一个有界退出窗口，避免 db.Close() 吃掉收尾写入
	drained := make(chan struct{})
	go func() {
		inFlight.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		log.Printf("[main] shutdown: in-flight handlers still draining after 30s; proceeding")
	}
	// 先停掉后台任务派生者并等它们收尾（调度器计划、自动领取、额度刷新轮），
	// 再等 relay 派生的后台任务。三者的 Stop 都幂等（stopOnce + 有界 join），
	// 末尾 defer 再调一次是空操作
	scheduler.Stop()
	autoClaim.Stop()
	pool.Stop()
	// relay 派生的后台任务（额度刷新/自动重置）会在 handler 返回后继续跑：
	// WaitBackground 先拒绝新任务（含 >30s 排水窗口里残存的 handler 迟到
	// spawn，它们写库只会失败并被日志记录，不会撞 WaitGroup 契约）再有界等待，
	// 终态写库不会撞上已关闭的库被静默吞掉——稀缺重置槽就白烧了
	zapi.WaitBackground(20 * time.Second)
}

// listenAddrIsLoopback 监听地址是否只绑定环回（localhost / 127.0.0.0/8 / ::1）。
// 通配（空 host、0.0.0.0、::、*）= 对外暴露，按非环回处理。
func listenAddrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "", "*", "0.0.0.0", "::":
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// limitBody 全局请求体上限：管理 API 与登录接口此前无大小限制，
// 超大 JSON 会在 Decode 时整体载入内存（未认证 /api/login 即可触发）。
// /v1 转发路径另有 8MB 的 readJSONBody 上限，互不影响。
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
		}
		next.ServeHTTP(w, r)
	})
}

// writeTracker 记录响应是否已开写（panic 兜底据此决定补 502 还是静默）。
// Flush 必须透传：SSE 路径靠 w.(http.Flusher) 断言刷新，包一层丢了接口
// 流式就整个哑掉
type writeTracker struct {
	http.ResponseWriter
	wrote bool
}

func (wt *writeTracker) WriteHeader(code int) {
	wt.wrote = true
	wt.ResponseWriter.WriteHeader(code)
}

func (wt *writeTracker) Write(b []byte) (int, error) {
	wt.wrote = true
	return wt.ResponseWriter.Write(b)
}

func (wt *writeTracker) Flush() {
	if f, ok := wt.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
