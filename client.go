package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"golang.org/x/net/proxy"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ---- ZCode 桌面客户端身份伪装 ----
// 移植 zcode-switch quota.rs 的 zai_headers_with_version：
// 计费/活动/聊天接口共用同一套客户端标识头。

const (
	zcodeOrigin       = "https://zcode.z.ai"
	zcodeLang         = "zh-CN"
	zcodeChannel      = "stable"
	fallbackAppVer    = "3.14.4"
	screenResolution  = "2560x1440"
	anthropicVersionH = "2023-06-01"
)

var (
	clientInfoOnce sync.Once
	cachedPlatform string
	cachedTZ       string
	cachedOSVer    string
	cachedOSCat    string

	// 上游 transport 缓存（连接池复用）：按 (代理, 类型, 指纹) 共享连接池。
	// http.Client 按需廉价构造（Timeout 是 client 级属性），不复用 transport
	// 的话 8 种超时值会把同一主机的连接池撕成 8 份，保活连接形同虚设
	transportCache sync.Map
)

// ClientPlatform 返回 "win32-x64" 形式的平台标识
func ClientPlatform() string {
	clientInfoOnce.Do(initClientInfo)
	return cachedPlatform
}

// clientTimezoneValue 当前时区（IANA 名）
func clientTimezoneValue() string {
	clientInfoOnce.Do(initClientInfo)
	return cachedTZ
}

// osCategoryValue 操作系统类别（windows/darwin/linux）
func osCategoryValue() string {
	clientInfoOnce.Do(initClientInfo)
	return cachedOSCat
}

func initClientInfo() {
	osName := NodePlatform()
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	}
	cachedPlatform = osName + "-" + arch
	cachedOSCat = runtime.GOOS // windows / darwin / linux
	cachedTZ = detectTimezone()
	cachedOSVer = detectOSVersion()
}

// detectTimezone Windows 用 tzutil 映射，其他平台取 /etc/localtime
func detectTimezone() string {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tzutil", "/g").Output()
		if err == nil {
			switch strings.TrimSpace(string(out)) {
			case "China Standard Time", "China Daylight Time":
				return "Asia/Shanghai"
			case "Singapore Standard Time":
				return "Asia/Singapore"
			case "Tokyo Standard Time":
				return "Asia/Tokyo"
			case "UTC":
				return "UTC"
			}
		}
		return "Asia/Shanghai"
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if tz := strings.TrimSpace(string(b)); tz != "" {
			return tz
		}
	}
	// macOS 无 /etc/timezone：/etc/localtime 是指向 zoneinfo 的符号链接
	if tgt, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.LastIndex(tgt, "/zoneinfo/"); i >= 0 {
			if tz := tgt[i+len("/zoneinfo/"):]; tz != "" {
				return tz
			}
		}
	}
	return "UTC"
}

// detectOSVersion Windows 读注册表 CurrentBuildNumber → "10.0.x"
func detectOSVersion() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	out, err := exec.Command("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "/v", "CurrentBuildNumber").Output()
	if err != nil {
		return "10.0.19044"
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "CurrentBuildNumber") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				return "10.0." + fields[len(fields)-1]
			}
		}
	}
	return "10.0.19044"
}

// DetectZCodeAppVersion 探测已安装 ZCode 客户端版本：Windows 读注册表卸载信息
// （zcode-switch 同款逻辑），macOS 读 /Applications/ZCode.app 的 Info.plist，
// 找不到时回退内置版本号（与官方 zai-org/ZCode 当前发布版本对齐）。
func DetectZCodeAppVersion() string {
	switch runtime.GOOS {
	case "windows":
		if v := detectWindowsAppVersion(); v != "" {
			return v
		}
	case "darwin":
		if v := detectDarwinAppVersion(); v != "" {
			return v
		}
	}
	return fallbackAppVer
}

// detectDarwinAppVersion 从应用包 Info.plist 读取 CFBundleShortVersionString
func detectDarwinAppVersion() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	paths := []string{
		"/Applications/ZCode.app/Contents/Info.plist",
		home + "/Applications/ZCode.app/Contents/Info.plist",
	}
	re := regexp.MustCompile(`CFBundleShortVersionString</key>\s*<string>([0-9]+(?:\.[0-9]+)+)</string>`)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if m := re.FindSubmatch(data); m != nil {
			return normalizeVersion(string(m[1]))
		}
	}
	return ""
}

func detectWindowsAppVersion() string {
	hives := []string{
		`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
		`HKLM\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
		`HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
	}
	for _, hive := range hives {
		out, err := exec.Command("reg", "query", hive, "/s").Output()
		if err != nil {
			continue
		}
		var name, ver string
		for _, line := range strings.Split(string(out), "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "HKEY_") {
				if isZcodeDisplayName(name) && ver != "" {
					return normalizeVersion(ver)
				}
				name, ver = "", ""
				continue
			}
			// 行格式: "    DisplayName    REG_SZ    ZCode 3.11.2"
			fields := strings.Fields(l)
			if len(fields) < 3 {
				continue
			}
			switch fields[0] {
			case "DisplayName":
				name = strings.Join(fields[2:], " ")
			case "DisplayVersion":
				ver = fields[len(fields)-1]
			}
		}
		if isZcodeDisplayName(name) && ver != "" {
			return normalizeVersion(ver)
		}
	}
	return ""
}

func stripPrefix(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, prefix) {
		return strings.TrimPrefix(s, prefix), true
	}
	return "", false
}

func isZcodeDisplayName(name string) bool {
	l := strings.ToLower(name)
	return strings.Contains(l, "zcode") && !strings.Contains(l, "switch")
}

func normalizeVersion(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) >= 3 {
		return parts[0] + "." + parts[1] + "." + parts[2]
	}
	return v
}

// ClientIdentity 一次请求的客户端身份（版本 + 设备 + 请求 ID）
type ClientIdentity struct {
	AppVersion string
	DeviceMid  string
	RequestID  string
}

// NewClientIdentity 构造身份；deviceMid 为空时尝试读本机 telemetry-state.json
func NewClientIdentity(appVersion, deviceMid string) ClientIdentity {
	if deviceMid == "" {
		deviceMid = LocalDeviceMid()
	}
	return ClientIdentity{AppVersion: appVersion, DeviceMid: deviceMid, RequestID: uuid.NewString()}
}

// LocalDeviceMid 读取本机 ZCode 客户端的 deviceMid
func LocalDeviceMid() string {
	p := LocalTelemetryPath()
	if p == "" {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var v struct {
		DeviceMid string `json:"deviceMid"`
	}
	if json.Unmarshal(data, &v) != nil {
		return ""
	}
	return v.DeviceMid
}

// ZaiClientHeaders 生成 zcode.z.ai 计费/活动接口的完整客户端头
// （quota.rs zai_headers_with_version 移植，Authorization 由调用方补充）
func ZaiClientHeaders(id ClientIdentity) map[string]string {
	clientInfoOnce.Do(initClientInfo)
	h := map[string]string{
		"User-Agent":          "ZCode/" + id.AppVersion,
		"HTTP-Referer":        zcodeOrigin,
		"X-Title":             "Z Code@electron",
		"X-ZCode-App-Version": id.AppVersion,
		"X-Platform":          cachedPlatform,
		"X-Release-Channel":   zcodeChannel,
		"X-Client-Language":   zcodeLang,
		"X-Client-Timezone":   cachedTZ,
		"X-Os-Category":       cachedOSCat,
		"x-request-id":        id.RequestID,
		"Content-Type":        "application/json",
	}
	if cachedOSVer != "" {
		h["X-Os-Version"] = cachedOSVer
	}
	if id.DeviceMid != "" {
		h["X-Device-Mid"] = id.DeviceMid
	}
	return h
}

// ---- HTTP 客户端工厂（组代理 + 后续 utls 指纹）----

// proxyURLForNode 将代理节点转成 URL 字符串
func ProxyURLForNode(n *ProxyNode) string {
	if n == nil || n.Host == "" {
		return ""
	}
	scheme := n.Type
	if scheme == "" {
		scheme = "socks5"
	}
	auth := ""
	if n.Username != "" {
		auth = url.UserPassword(n.Username, n.Password).String() + "@"
	}
	return fmt.Sprintf("%s://%s%s:%d", scheme, auth, n.Host, n.Port)
}

// NewUpstreamHTTPClient 标准库 TLS 客户端（含 HTTP/2），用于 api.z.ai / open.bigmodel.cn
// 等非 ESA WAF 保护的端点（实测 api.z.ai 协商 h2）。
// cachedTransport 取共享 transport（不存在则构建并缓存）。同一 (代理, 类型, 指纹)
// 的所有 client 共用连接池；指纹/代理设置变更由 CloseIdleClients 整体失效。
//
// 复查结论（评审 F8，无需改码）：缓存键（"std|"+proxyURL / "fp|"+mode|ja3|proxyURL）
// 与 build 闭包捕获的配置来自同一份入参——配置变更后新请求算出新键必然 miss
// 重建，CloseIdleClients 与 LoadOrStore 的竞争最多留下一个孤儿旧 transport
// （不可达，GC 回收），绝不会把旧代理/旧指纹的 transport 发给变更后的请求。
// 因此不需要代际计数器；CloseIdleClients 清掉的只是旧键条目 + 空闲连接。
func cachedTransport(key string, build func() *http.Transport) *http.Transport {
	if v, ok := transportCache.Load(key); ok {
		return v.(*http.Transport)
	}
	actual, _ := transportCache.LoadOrStore(key, build())
	return actual.(*http.Transport)
}

// 上游 transport 首字节上限分两档：
//   - 流式 60s：SSE 只受 time-to-first-header 约束，黑洞路由不能无限占住账号并发闸门
//   - 非流式 10min：TTFB ≈ 上游完整思考+生成时长（GLM 思考型非流式调用：zcode 客户端的
//     验证/compaction 回退/提交信息生成会等数分钟）；复用流式档会在思考中途杀请求，
//     客户端把 5xx 整单重试并白白烧账号
const (
	upstreamHeaderTimeout = 60 * time.Second
	slowTTFBHeaderTimeout = 10 * time.Minute
)

// NewUpstreamHTTPClient 标准库 TLS 客户端（含 HTTP/2），用于 api.z.ai / open.bigmodel.cn
// 等非 ESA WAF 保护的端点（实测 api.z.ai 协商 h2）。
func NewUpstreamHTTPClient(proxyURL string, timeout time.Duration) *http.Client {
	t := cachedTransport("std|"+proxyURL, func() *http.Transport {
		return buildStdTransport(proxyURL, upstreamHeaderTimeout)
	})
	return clientWithTransport(t, timeout)
}

// NewUpstreamHTTPClientSlowTTFB 非流式转发专用：首字节上限放宽到 slowTTFBHeaderTimeout，
// client 级 Timeout 恒为 0（响应体读取不受限，与流式同规靠 context）
func NewUpstreamHTTPClientSlowTTFB(proxyURL string) *http.Client {
	t := cachedTransport("std-slow|"+proxyURL, func() *http.Transport {
		return buildStdTransport(proxyURL, slowTTFBHeaderTimeout)
	})
	return clientWithTransport(t, 0)
}

func buildStdTransport(proxyURL string, headerTimeout time.Duration) *http.Transport {
	transport := &http.Transport{
		// 拨号/响应头都有界：流式客户端 Timeout=0 时没有它们，黑洞路由
		// 会占住账号并发闸门直到下游断开（SSE 只受 time-to-first-header 约束，不受影响）
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: headerTimeout,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16, // Go 默认 2：并发下多余连接被关闭，每请求重握手直拉高 TTFB
		IdleConnTimeout:       90 * time.Second,
	}
	applyProxy(transport, proxyURL)
	return transport
}

func clientWithTransport(t http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: t,
		Timeout:   timeout,
		// 不跟随重定向：WAF 挑战/登录页 302 交由 relay 显式分类
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// NewFingerprintHTTPClient utls 指纹客户端（HTTP/1.1），用于 zcode.z.ai 等
// ESA WAF 保护端点：模拟浏览器 ClientHello，且 WAF 不支持 h2 ALPN。
func NewFingerprintHTTPClient(proxyURL string, timeout time.Duration) *http.Client {
	fp := fingerprintHook()
	t := cachedTransport("fp|"+fp.Mode+"|"+fp.JA3+"|"+proxyURL, func() *http.Transport {
		return buildFingerprintTransport(proxyURL, fp, upstreamHeaderTimeout)
	})
	return clientWithTransport(t, timeout)
}

// NewFingerprintHTTPClientSlowTTFB 非流式转发专用（同 NewUpstreamHTTPClientSlowTTFB）
func NewFingerprintHTTPClientSlowTTFB(proxyURL string) *http.Client {
	fp := fingerprintHook()
	t := cachedTransport("fp-slow|"+fp.Mode+"|"+fp.JA3+"|"+proxyURL, func() *http.Transport {
		return buildFingerprintTransport(proxyURL, fp, slowTTFBHeaderTimeout)
	})
	return clientWithTransport(t, 0)
}

func buildFingerprintTransport(proxyURL string, fp TLSFingerprint, headerTimeout time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	dialTLS := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host := addr
		if h, _, err := net.SplitHostPort(addr); err == nil {
			host = h
		}
		raw, err := dialRaw(ctx, dialer, proxyURL, network, addr)
		if err != nil {
			return nil, err
		}
		return utlsHandshake(ctx, raw, host, fp)
	}
	return &http.Transport{
		DialContext:    dialer.DialContext,
		DialTLSContext: dialTLS,
		// 黑洞路由（代理/TCP 通了但对端永不回包）在 Timeout=0 的流式请求上
		// 只受此约束——缺失时一次挂起就占死账号并发闸门直到下游断开
		ResponseHeaderTimeout: headerTimeout,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // 禁 h2
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16, // utls 握手成本高，保活连接直接决定 TTFB 稳定性
		IdleConnTimeout:       90 * time.Second,
	}
}

// ClientForURL 按主机选择客户端：zcode.z.ai（ESA WAF）→ utls 指纹；其余 → 标准库 h2。
// transport 按 (代理, 类型, 指纹, TTFB 档) 共享连接池；client（含 Timeout）按需构造。
// 指纹/代理设置变更时 CloseIdleClients() 整体失效。
func ClientForURL(proxyURL, urlStr string, timeout time.Duration) *http.Client {
	if strings.Contains(urlStr, "zcode.z.ai") {
		return NewFingerprintHTTPClient(proxyURL, timeout)
	}
	return NewUpstreamHTTPClient(proxyURL, timeout)
}

// ClientForURLSlowTTFB 非流式转发专用（relay）：首字节可等待至 slowTTFBHeaderTimeout
func ClientForURLSlowTTFB(proxyURL, urlStr string) *http.Client {
	if strings.Contains(urlStr, "zcode.z.ai") {
		return NewFingerprintHTTPClientSlowTTFB(proxyURL)
	}
	return NewUpstreamHTTPClientSlowTTFB(proxyURL)
}

// CloseIdleClients 关闭并清空缓存的全部上游 transport（指纹/代理设置变更后调用）
func CloseIdleClients() {
	transportCache.Range(func(k, v interface{}) bool {
		v.(*http.Transport).CloseIdleConnections()
		transportCache.Delete(k)
		return true
	})
}

// applyProxy 给标准 transport 配置代理
func applyProxy(transport *http.Transport, proxyURL string) {
	if proxyURL == "" {
		return
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		// 解析失败退直连是出口隐私 fail-open：老版本/手改库的坏值必须大声报，
		// 否则 UI 显示有代理而流量全走本机 IP
		log.Printf("[egress] malformed proxy URL %q: %v; falling back to DIRECT", proxyURL, err)
		return
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		if dialer, derr := Socks5Dialer(u); derr == nil {
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.(proxyContextDialer).DialContext(ctx, network, addr)
			}
		}
	default:
		transport.Proxy = http.ProxyURL(u)
	}
}

// dialRaw 建立到目标 addr 的原始 TCP 连接（经代理隧道或直连）
func dialRaw(ctx context.Context, dialer *net.Dialer, proxyURL, network, addr string) (net.Conn, error) {
	if proxyURL == "" {
		return dialer.DialContext(ctx, network, addr)
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		// 与 applyProxy 同理：解析失败退直连是出口隐私 fail-open，必须大声报
		// （此处是 zcode.z.ai 主转发的指纹通道，静默绕行零痕迹）
		log.Printf("[egress] malformed proxy URL %q: %v; falling back to DIRECT", proxyURL, err)
		return dialer.DialContext(ctx, network, addr)
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		sd, err := Socks5Dialer(u)
		if err != nil {
			return nil, err
		}
		// 必须走 ContextDialer：旧 Dial 内部用 context.Background()，
		// 代理握手不响应时会永久泄漏 goroutine 和连接（ctx 取消救不了它）
		if cd, ok := sd.(proxy.ContextDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		return sd.Dial(network, addr)
	default: // http/https 代理：CONNECT 隧道
		conn, err := dialer.DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		// https 代理 = 先对代理端口做 TLS，再在其上发 CONNECT（与 stdlib
		// http.ProxyURL 对 https 代理的语义一致）：明文 CONNECT 打 TLS 端口
		// 会被代理拒绝，整条指纹通道全灭而健康检查（走 stdlib）却显示正常
		if u.Scheme == "https" {
			tconn := tls.Client(conn, &tls.Config{
				ServerName:         u.Hostname(),
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true, // 代理端证书常为自签；与上游 TLS 校验无关
			})
			if err := tconn.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, fmt.Errorf("proxy TLS handshake: %w", err)
			}
			conn = tconn
		}
		if err := httpConnectTunnel(ctx, conn, addr, u); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

// httpConnectTunnel 向 HTTP 代理发送 CONNECT 并等待 200
func httpConnectTunnel(ctx context.Context, conn net.Conn, addr string, proxyURL *url.URL) error {
	// CONNECT 握手（写请求 + 读响应）必须有界：代理接受 TCP 后不回包时，
	// ReadResponse 会永久阻塞且 ctx 取消救不了它——用看门狗关连接
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	watchdog := make(chan struct{})
	defer conn.SetDeadline(time.Time{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-watchdog:
		}
	}()
	defer close(watchdog)
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		pass, _ := proxyURL.User.Password()
		req.Header.Set("Proxy-Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username()+":"+pass)))
	}
	if err := req.Write(conn); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("代理 CONNECT 失败: HTTP %d", resp.StatusCode)
	}
	if br.Buffered() > 0 {
		return fmt.Errorf("CONNECT 响应带多余数据")
	}
	return nil
}
