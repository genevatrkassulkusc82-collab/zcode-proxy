package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
	db, err := NewDB(absDBPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	log.Printf("[main] database: %s", absDBPath)

	// -doctor：离线体检（配置/库完整性/vault/账号/代理/Key），不启动任何服务
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
	log.Printf("[main] zcode-proxy listening on http://%s", listenAddr)
	log.Printf("[main] web UI: http://%s/web", listenAddr)
	// 显式 Server：ReadHeaderTimeout 防 Slowloris；SSE 决定不设 WriteTimeout
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           limitBody(auth.Middleware(mux)),
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
		log.Printf("[main] shutdown: %v", err)
	}
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
