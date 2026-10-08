package main

// URL 形态图片内联（image Fetch → base64）。
//
// 背景：上游只接受 base64 source，此前 url 形态一律本地 400。zai-org/ZCode 当前
// 总是发 base64（自行读文件/下载），但 AI SDK 层支持 url 形态图片块，为兼容性
// 预留受控抓取：仅 http(s)、连接前 IP 校验（SSRF：禁私网/环回/链路本地/CGNAT）、
// 直连不走代理（代理会绕过 Control 的连接前校验）、限长限时、content-type 白名单。
// 任何失败 fail-closed 回 400，与原行为一致。

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const (
	maxImageFetchBytes = 5 << 20 // 5MB raw ≈ 6.7MB base64，与上游图片上限同量级
	imageFetchTimeout  = 15 * time.Second
	// 单请求 URL 图片内联上限（评审轮 3 红队 F1：无上限时 40 个 ~80B 的 url 块
	// 可放大出 213MB base64 —— 内存放大 68000×，OOM 可达）
	maxInlineImagesPerRequest      = 24
	maxInlineBase64BytesPerRequest = 32 << 20
)

// imageInlineBudget 单请求内联额度；URL 去重缓存让同一 URL 只抓一次
type imageInlineBudget struct {
	count int
	bytes int
	cache map[string]inlinedImage
}

func newImageInlineBudget() *imageInlineBudget {
	return &imageInlineBudget{cache: map[string]inlinedImage{}}
}

type inlinedImage struct {
	mediaType string
	data      string // base64
}

// imageFetchHTTPClient 测试注入点：默认带 SSRF 防护
var imageFetchHTTPClient = newGuardedImageFetchClient()

func newGuardedImageFetchClient() *http.Client {
	return &http.Client{
		Timeout: imageFetchTimeout,
		Transport: &http.Transport{
			// Control 在 DNS 解析后、连接建立前运行：DNS 解析出私网地址同样被拦
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
				Control: func(_, address string, _ syscall.RawConn) error {
					host, _, err := net.SplitHostPort(address)
					if err != nil {
						return err
					}
					ip := net.ParseIP(host)
					if ip == nil {
						return fmt.Errorf("non-IP dial address %q", host)
					}
					if isForbiddenImageHostIP(ip) {
						return fmt.Errorf("refusing to fetch image from local/private address %s", ip)
					}
					return nil
				},
			}).DialContext,
			// 显式不使用代理环境变量：经代理转发会绕过上面的连接前 IP 校验
			Proxy: nil,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to non-http(s) url (scheme %q)", req.URL.Scheme)
			}
			return nil
		},
	}
}

// isForbiddenImageHostIP SSRF 目标黑名单：环回/私网/未指定/链路本地/多播/ULA/CGNAT
func isForbiddenImageHostIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil { // 100.64.0.0/10 CGNAT（IsPrivate 不覆盖）
		return v4[0] == 100 && v4[1] >= 64 && v4[1] < 128
	}
	return false
}

// fetchImageAsBase64 绑定 ctx：客户端断开时抓取随之中止（评审轮 3 红队 F2）
func fetchImageAsBase64(ctx context.Context, rawURL string) (inlinedImage, error) {
	if rawURL == "" {
		return inlinedImage{}, errors.New("empty url")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return inlinedImage{}, errors.New("only absolute http(s) urls are supported")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return inlinedImage{}, err
	}
	resp, err := imageFetchHTTPClient.Do(req)
	if err != nil {
		return inlinedImage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return inlinedImage{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 严格 content-type 白名单（Anthropic 契约仅收 jpeg/png/gif/webp）：
	// 不做嗅探降级，缺头/歧义一律拒绝
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return inlinedImage{}, fmt.Errorf("unsupported content-type %q (want image/jpeg|png|gif|webp)", mediaType)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxImageFetchBytes+1))
	if err != nil {
		return inlinedImage{}, fmt.Errorf("read body: %v", err)
	}
	if len(raw) > maxImageFetchBytes {
		return inlinedImage{}, fmt.Errorf("image exceeds %d byte limit", maxImageFetchBytes)
	}
	if len(raw) == 0 {
		return inlinedImage{}, errors.New("empty body")
	}
	return inlinedImage{mediaType: mediaType, data: base64.StdEncoding.EncodeToString(raw)}, nil
}
