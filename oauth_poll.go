package main

// ---- 服务端中介的 CLI 轮询登录（B1，移植 zcode-api src/auth/oauth.ts PollOAuthClient）----
//
// 与官方桌面端 3.12.3 startOAuthWithPolling 一致：
//   1. 生成 32 字节 hex pollToken，作为 Bearer 同时用于 init 与 poll；
//   2. POST /api/v1/oauth/cli/init  {provider:"zai"} → {flow_id, authorize_url, expires_at, poll_interval_sec}；
//   3. 打开 authorize_url 并附加桌面中转参数（zai: redirect_uri / bigmodel: redirect）——
//      中转页 https://zcode.z.ai/app/oauth/login?redirect=zcode://oauth/callback&app_version=X
//      在服务端记录授权结果，浏览器不回连 localhost（无需注册 redirect_uri）；
//   4. GET /api/v1/oauth/cli/poll/{flow_id} 轮询至 status=ready → {token(JWT), user, zai:{access_token}}。
//
// 错误语义（对齐 bundle）：4xx（除 408/429）、信封 code!==0、未知 status、
// 畸形 200 → 终止；网络错误 / 5xx / 408 / 429 → 视为 pending 继续轮询。
// 总超时 = min(5 分钟, expires_at)。

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

const (
	cliLoginInitURL  = "https://zcode.z.ai/api/v1/oauth/cli/init"
	cliLoginPollBase = "https://zcode.z.ai/api/v1/oauth/cli/poll"
	pollLoginTimeout = 300 * time.Second
)

// StartPollLogin 发起免回调轮询登录；返回 (flow, 授权页 URL)。
// 轮询在后台 goroutine 进行，UI 通过既有 FlowStatus(state) 轮询结果。
func (m *OAuthManager) StartPollLogin(group string) (*OAuthFlow, string, error) {
	pollToken := make([]byte, 32)
	if _, err := rand.Read(pollToken); err != nil {
		return nil, "", fmt.Errorf("生成 poll token 失败: %w", err)
	}
	tokenHex := hex.EncodeToString(pollToken)

	initData, err := m.cliInit(tokenHex)
	if err != nil {
		return nil, "", fmt.Errorf("cli/init 失败: %w", err)
	}
	flowID := jsonStr(initData, "flow_id")
	authorizeURL := jsonStr(initData, "authorize_url")
	expiresAt := int64(jsonInt(initData, "expires_at"))
	pollIntervalSec := jsonInt(initData, "poll_interval_sec")
	if flowID == "" || authorizeURL == "" {
		return nil, "", fmt.Errorf("cli/init 响应缺少 flow_id/authorize_url")
	}
	if au, err := url.Parse(authorizeURL); err != nil || au.Scheme != "https" || au.Host == "" {
		// 对齐官方 cli-oauth.ts：authorize_url 必须 https，防被引导到 http:/javascript:
		return nil, "", fmt.Errorf("authorize_url 非法（必须为 https 绝对地址）")
	}
	if pollIntervalSec < 1 {
		pollIntervalSec = 2
	}

	finalURL, err := applyDesktopInterstitial(authorizeURL, "zai", m.zapi.appVersion)
	if err != nil {
		return nil, "", fmt.Errorf("构造授权 URL 失败: %w", err)
	}

	flow := &OAuthFlow{
		State:        flowID,
		RedirectURI:  "", // 无本地回调
		Manual:       false,
		Group:        group,
		CreatedAt:    time.Now().Unix(),
		Status:       "pending",
		Message:      "等待授权（授权完成后自动完成）…",
		AuthorizeURL: finalURL,
	}
	m.mu.Lock()
	m.flows[flowID] = flow
	for k, f := range m.flows {
		if time.Now().Unix()-f.CreatedAt > 600 {
			delete(m.flows, k)
		}
	}
	m.mu.Unlock()

	// 轮询间隔来自上游响应：夹取防止畸形值导致挂死或热旋
	interval := time.Duration(pollIntervalSec) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 15*time.Second {
		interval = 15 * time.Second
	}
	go m.pollLoop(flow, tokenHex, flowID, expiresAt, interval)
	return flow, finalURL, nil
}

// applyDesktopInterstitial 附加桌面中转参数（provider=zai → redirect_uri）
func applyDesktopInterstitial(authorizeURL, provider, appVersion string) (string, error) {
	u, err := url.Parse(authorizeURL)
	if err != nil {
		return "", err
	}
	interstitial := "https://zcode.z.ai/app/oauth/login?redirect=" + url.QueryEscape("zcode://oauth/callback") +
		"&app_version=" + url.QueryEscape(appVersion)
	if provider == "bigmodel" {
		q := u.Query()
		q.Set("redirect", interstitial)
		u.RawQuery = q.Encode()
	} else {
		q := u.Query()
		q.Set("redirect_uri", interstitial)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// cliInit POST /oauth/cli/init
func (m *OAuthManager) cliInit(pollToken string) (map[string]interface{}, error) {
	payload, _ := json.Marshal(map[string]string{"provider": "zai"})
	req, err := http.NewRequest("POST", cliLoginInitURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Content-Type", "application/json")
	// cli/* 与消息通道同属 zcode.z.ai（ESA WAF），统一走指纹客户端
	client := ClientForURL(m.zapi.egress.GlobalProxyURL(), cliLoginInitURL, 15*time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return unwrapZcodeEnvelope(resp)
}

// pollLoop 轮询直至 ready / failed / 超时；ready 时直接入库（复用 completeFlow 的摄取逻辑）
func (m *OAuthManager) pollLoop(flow *OAuthFlow, pollToken, flowID string, expiresAt int64, interval time.Duration) {
	deadline := time.Now().Add(pollLoginTimeout)
	if expiresAt > 0 {
		if e := time.Unix(expiresAt, 0); e.Before(deadline) {
			deadline = e
		}
	}
	// cli/poll 与 cli/init 同属 zcode.z.ai（ESA WAF），统一走指纹客户端
	client := ClientForURL(m.zapi.egress.GlobalProxyURL(), cliLoginPollBase, 15*time.Second)

	for {
		if time.Now().After(deadline) {
			m.finishFlow(flow, "", "授权超时，请重新发起登录")
			return
		}
		ready, data, fatal := m.pollOnce(client, pollToken, flowID)
		if fatal != nil {
			m.finishFlow(flow, "", "授权失败: "+fatal.Error())
			log.Printf("[oauth] poll %s… fatal: %v", safePrefixLog(flowID, 8), fatal)
			return
		}
		if ready {
			m.setFlowStatus(flow, "exchanging", "授权完成，正在入库…")
			if err := m.ingestTokens(flow, data); err != nil {
				m.finishFlow(flow, "", err.Error())
				log.Printf("[oauth] poll %s… ingest: %v", safePrefixLog(flowID, 8), err)
				return
			}
			return
		}
		time.Sleep(interval)
	}
}

// pollOnce 单轮轮询。返回 (ready, data, fatalErr)；
// 仅网络错误/5xx/408/429 视为 pending（对齐官方 auth-login-polling.ts：
// 非 408/429/5xx 一律终止——畸形 200、缺 data、未知 status、3xx 均为 fatal）。
func (m *OAuthManager) pollOnce(client *http.Client, pollToken, flowID string) (bool, map[string]interface{}, error) {
	req, err := http.NewRequest("GET", cliLoginPollBase+"/"+flowID, nil)
	if err != nil {
		return false, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	resp, err := client.Do(req)
	if err != nil {
		return false, nil, nil // 网络错误 → pending
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return false, nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// 客户端不跟随重定向：3xx 多为 WAF 挑战页，按终止处理而非空转满轮询窗口
		return false, nil, fmt.Errorf("HTTP %d（重定向/WAF 挑战）", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, nil, nil // 5xx 等 → pending
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v map[string]interface{}
	if json.Unmarshal(body, &v) != nil {
		return false, nil, fmt.Errorf("响应非 JSON（HTTP %d）", resp.StatusCode)
	}
	if codeVal, has := v["code"]; has {
		n := jsonInt(v, "code")
		if n != 0 {
			return false, nil, fmt.Errorf("业务码 %d: %s", n, firstNonEmpty(jsonStr(v, "msg"), jsonStr(v, "message")))
		}
		_ = codeVal
	}
	data, _ := v["data"].(map[string]interface{})
	if data == nil {
		return false, nil, fmt.Errorf("轮询响应缺少 data")
	}
	switch jsonStr(data, "status") {
	case "ready":
		return true, data, nil
	case "pending":
		return false, nil, nil
	case "failed":
		return false, nil, fmt.Errorf("服务端报告授权失败")
	default: // 未知状态按终止处理（对齐官方）
		return false, nil, fmt.Errorf("未知授权状态: %q", jsonStr(data, "status"))
	}
}

// unwrapZcodeEnvelope {code,data,msg} 信封解包（数值 code 必需）
func unwrapZcodeEnvelope(resp *http.Response) (map[string]interface{}, error) {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v map[string]interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("响应非 JSON（HTTP %d）", resp.StatusCode)
	}
	code, has := v["code"].(float64)
	if !has {
		return nil, fmt.Errorf("响应缺少数值 code（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || int(code) != 0 {
		msg := firstNonEmpty(jsonStr(v, "msg"), jsonStr(v, "message"), truncate(string(body), 200))
		return nil, fmt.Errorf("HTTP %d code %d: %s", resp.StatusCode, int(code), msg)
	}
	data, _ := v["data"].(map[string]interface{})
	if data == nil {
		return nil, fmt.Errorf("响应缺少 data")
	}
	return data, nil
}
