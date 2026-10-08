package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// ---- 出口代理：SOCKS5 拨号器 / 组代理解析 / 健康测试 / 系统代理探测 ----

type proxyContextDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// Socks5Dialer 从 socks5://[user:pass@]host:port 创建支持 context 的拨号器
func Socks5Dialer(u *url.URL) (proxy.Dialer, error) {
	var auth *proxy.Auth
	if u.User != nil {
		auth = &proxy.Auth{User: u.User.Username()}
		if p, ok := u.User.Password(); ok {
			auth.Password = p
		}
	}
	forward := &net.Dialer{Timeout: 15 * time.Second}
	return proxy.SOCKS5("tcp", u.Host, auth, forward)
}

// EgressProxy 出口代理解析器：组绑定代理 → 默认代理 → 全局设置 → 直连
type EgressProxy struct {
	db *DB
}

// NewEgressProxy 创建解析器
func NewEgressProxy(db *DB) *EgressProxy {
	return &EgressProxy{db: db}
}

// ProxyURLForAccount 解析账号应走的出口代理 URL（空=直连）
func (e *EgressProxy) ProxyURLForAccount(a *Account) string {
	if a == nil {
		return e.GlobalProxyURL()
	}
	node, err := e.db.ProxyNodeForGroup(a.AccountGroup)
	if err != nil {
		// 解析失败静默回退会让钉住组的出口 IP 悄悄换成全局/直连——
		// 恰是触发上游风控阶梯的模式，必须留痕
		log.Printf("[egress] WARNING: group %q proxy node lookup failed: %v — falling back to global/direct", a.AccountGroup, err)
	} else if node != nil {
		if u := ProxyURLForNode(node); u != "" {
			return u
		}
	}
	return e.GlobalProxyURL()
}

// GlobalProxyURL 全局上游代理设置（settings KV upstream_proxy）
func (e *EgressProxy) GlobalProxyURL() string {
	v, _ := e.db.GetSetting("upstream_proxy")
	return strings.TrimSpace(v)
}

// ---- 健康测试：出口 IP ----

var exitIPAPIs = []string{
	"https://api.ipify.org/?format=json",
	"https://ipinfo.io/json",
}

// TestProxyExitIP 测试代理连通性并返回出口 IP（proxyURL 空=直连基线）
func TestProxyExitIP(proxyURL string) (ip string, elapsed time.Duration, err error) {
	client := NewUpstreamHTTPClient(proxyURL, 15*time.Second)
	start := time.Now()
	for _, api := range exitIPAPIs {
		resp, e := client.Get(api)
		if e != nil {
			err = e
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		var v map[string]interface{}
		if json.Unmarshal(body, &v) == nil {
			if s, ok := v["ip"].(string); ok && s != "" {
				return s, time.Since(start), nil
			}
		}
	}
	if err == nil {
		err = fmt.Errorf("无法获取出口 IP")
	}
	return "", time.Since(start), err
}

// ---- 系统代理探测（Windows 注册表）----

// DetectSystemProxy 读取 Windows 系统代理设置
func DetectSystemProxy() (enabled bool, proxyURL string) {
	if runtime.GOOS != "windows" {
		for _, env := range []string{"http_proxy", "HTTP_PROXY", "all_proxy", "ALL_PROXY"} {
			if v := strings.TrimSpace(os.Getenv(env)); v != "" {
				return true, v
			}
		}
		return false, ""
	}
	out, err := exec.Command("reg", "query",
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		"/v", "ProxyEnable").Output()
	if err != nil || !strings.Contains(string(out), "0x1") {
		return false, ""
	}
	out2, err := exec.Command("reg", "query",
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		"/v", "ProxyServer").Output()
	if err != nil {
		return false, ""
	}
	server := ""
	for _, line := range strings.Split(string(out2), "\n") {
		if strings.Contains(line, "ProxyServer") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				server = fields[len(fields)-1]
			}
		}
	}
	if server == "" {
		return false, ""
	}
	// ProxyServer 可能是 "host:port" 或 "http=...;https=...;socks=..."
	if strings.Contains(server, "=") {
		parts := map[string]string{}
		for _, p := range strings.Split(server, ";") {
			if kv := strings.SplitN(p, "=", 2); len(kv) == 2 {
				parts[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
			}
		}
		server = parts["https"]
		if server == "" {
			server = parts["http"]
		}
		if server == "" {
			for k, v := range parts {
				server = v
				// 仅 socks= 时按 SOCKS 语义回填 scheme：标成 http:// 会把
				// CONNECT 字节打到 SOCKS 端口，用户存下的建议值必然失败
				if k == "socks" {
					server = "socks5://" + v
				}
				break
			}
		}
	}
	if server != "" && !strings.Contains(server, "://") {
		server = "http://" + server
	}
	return true, server
}

// ---- 本机代理端口探测（zcode2api proxy.py 移植）----

var probePorts = []int{7897, 7890, 7891, 7899, 1080, 10808, 2080, 8889, 8118}

var portLabels = map[int]string{
	7897:  "Clash Verge 混合端口（常见默认）",
	7890:  "Clash 混合端口（常见默认）",
	7891:  "Clash HTTP 端口",
	7899:  "Clash Verge 备用端口",
	1080:  "SOCKS5 通用端口",
	10808: "v2rayN SOCKS 端口",
	2080:  "sing-box 混合端口",
	8889:  "HTTP 代理通用端口",
	8118:  "Privoxy HTTP 端口",
}

// ProbeLocalProxyPorts 并发探测本机常见代理内核端口
func ProbeLocalProxyPorts() []map[string]interface{} {
	type result struct {
		port int
		open bool
	}
	results := make(chan result, len(probePorts))
	for _, p := range probePorts {
		go func(port int) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
			if err == nil {
				conn.Close()
			}
			results <- result{port, err == nil}
		}(p)
	}
	var out []map[string]interface{}
	byPort := map[int]bool{}
	for i := 0; i < len(probePorts); i++ {
		r := <-results
		byPort[r.port] = r.open
	}
	for _, p := range probePorts {
		if byPort[p] {
			label := portLabels[p]
			if label == "" {
				label = "本机代理端口"
			}
			out = append(out, map[string]interface{}{
				"url":   fmt.Sprintf("http://127.0.0.1:%d", p),
				"port":  p,
				"label": label,
			})
		}
	}
	return out
}

// MaskProxyURL 脱敏展示代理地址（隐藏用户名密码）
func MaskProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	username := u.User.Username()
	masked := "***"
	if len(username) >= 2 {
		masked = username[:2] + "***"
	}
	u.User = url.User(masked)
	return u.String()
}
