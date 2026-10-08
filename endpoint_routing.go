package main

// ---- 服务端可控的端点路由表（B4，移植 zcode-api endpoint-routing.ts）----
//
// 桌面端 3.7+ 周期拉取 GET {origin}/api/v1/agent/configs，按返回的
// data.proxyEndpoint.mapping（from → to，https URL 精确匹配）重写上游请求 URL。
// 2026-08 起服务端把 coding-plan Anthropic 端点映射到 ultra[-zai] 路径；
// 表由服务端控制，随时可增长，因此做通用解析。
//
// 失败语义严格 fail-open：任何拉取/解析错误保留上一个快照（或无快照），
// 请求走原始 URL，并进入 30s 冷却。本实现绝不返回错误。

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	routingDefaultOrigin   = "https://zcode.z.ai"
	routingConfigPath      = "/api/v1/agent/configs"
	routingSuccessTTL      = 5 * time.Minute
	routingFailureCooldown = 30 * time.Second
	routingTimeout         = 3 * time.Second
	routingMaxEntries      = 256
)

var errInvalidMappingURL = errors.New("mapping URL is not a plain https URL")

// routingEntry 服务端 mapping 表项
type routingEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type routingSnapshot struct {
	expiresAt time.Time
	mapping   map[string]string
}

// EndpointRouter 服务端可控的 URL 重写表（进程级共享，fail-open）
type EndpointRouter struct {
	origin string

	mu        sync.Mutex
	snapshot  *routingSnapshot
	retryWait time.Time

	refreshMu   sync.Mutex
	testTimeout time.Duration // 测试注入：覆盖 3s 默认超时
}

// NewEndpointRouter 创建路由表服务
func NewEndpointRouter(origin string) *EndpointRouter {
	o := strings.TrimRight(strings.TrimSpace(origin), "/")
	if o == "" {
		o = routingDefaultOrigin
	}
	return &EndpointRouter{origin: o}
}

// routingKey 归一化 URL 为映射键：scheme://host:port + 去尾斜杠 path
func routingKey(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	path := u.Path
	if path == "" {
		path = "/"
	} else {
		path = strings.TrimRight(path, "/")
		if path == "" {
			path = "/"
		}
	}
	return u.Scheme + "://" + host + ":" + port + path
}

// Resolve 通过映射表解析上游 URL。永不失败：任何异常返回原始 URL。
// 快照缺失/过期时同步刷新（3s 超时 + 单飞），对齐 zcode-api ensureFresh 语义。
func (r *EndpointRouter) Resolve(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return rawURL
	}
	r.ensureFresh()
	r.mu.Lock()
	var target string
	if r.snapshot != nil {
		target = r.snapshot.mapping[routingKey(parsed)]
	}
	r.mu.Unlock()

	if target == "" {
		return rawURL
	}
	rewritten, err := url.Parse(target)
	if err != nil {
		return rawURL
	}
	rewritten.RawQuery = parsed.RawQuery
	return rewritten.String()
}

// ensureFresh 快照新鲜或冷却中直接返回；否则单飞同步刷新
func (r *EndpointRouter) ensureFresh() {
	r.mu.Lock()
	fresh := r.snapshot != nil && time.Now().Before(r.snapshot.expiresAt)
	cooling := time.Now().Before(r.retryWait)
	r.mu.Unlock()
	if fresh || cooling {
		return
	}
	// refreshMu 串行化刷新：排队 waiter 会阻塞至在途刷新结束（单飞语义），
	// 拿到锁后复查新鲜度——前一个 waiter 可能已完成刷新
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	r.mu.Lock()
	fresh = r.snapshot != nil && time.Now().Before(r.snapshot.expiresAt)
	cooling = time.Now().Before(r.retryWait)
	r.mu.Unlock()
	if fresh || cooling {
		return
	}
	r.refresh()
}

// Refresh 同步刷新（测试/后台任务用）
func (r *EndpointRouter) Refresh() { r.refresh() }

// routingGlobalProxyHook 由 main 注入：配置拉取与其余上游调用走同一网络路径
// （代理部署下直连会永久 markFailure，端点映射退回 legacy 路径）
var routingGlobalProxyHook = func() string { return "" }

func (r *EndpointRouter) refresh() {
	timeout := r.testTimeout
	if timeout <= 0 {
		timeout = routingTimeout
	}
	client := ClientForURL(routingGlobalProxyHook(), r.origin+routingConfigPath, timeout)
	req, err := http.NewRequest("GET", r.origin+routingConfigPath, nil)
	if err != nil {
		r.markFailure()
		return
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		r.markFailure()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		r.markFailure()
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		r.markFailure()
		return
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			ProxyEndpoint struct {
				Mapping []routingEntry `json:"mapping"`
			} `json:"proxyEndpoint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Code != 0 {
		r.markFailure()
		return
	}
	list := envelope.Data.ProxyEndpoint.Mapping
	if len(list) > routingMaxEntries {
		r.markFailure()
		return
	}
	mapping := make(map[string]string, len(list))
	for _, e := range list {
		from, err := parsePlainURL(e.From)
		if err != nil {
			r.markFailure()
			return
		}
		to, err := parsePlainURL(e.To)
		if err != nil {
			r.markFailure()
			return
		}
		key := routingKey(from)
		if _, dup := mapping[key]; dup {
			r.markFailure()
			return
		}
		mapping[key] = to.String()
	}
	r.mu.Lock()
	r.snapshot = &routingSnapshot{expiresAt: time.Now().Add(routingSuccessTTL), mapping: mapping}
	r.mu.Unlock()
	log.Printf("[routing] endpoint mapping updated: %d entries", len(mapping))
}

func (r *EndpointRouter) markFailure() {
	r.mu.Lock()
	r.retryWait = time.Now().Add(routingFailureCooldown)
	r.mu.Unlock()
}

// parsePlainURL mapping 表项必须是纯 https URL（无 userinfo/query/fragment）
func parsePlainURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return nil, errInvalidMappingURL
	}
	return u, nil
}
