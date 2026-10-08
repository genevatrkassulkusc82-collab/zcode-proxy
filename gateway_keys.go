package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ---- 网关 Key 运行时（R1）----
// 认证（auth.Middleware）解析请求 Key → 命中命名 Key 时做 RPM/启停检查并注入
// context；转发层（relay）用 context 里的 Key 做模型白名单/配额拦截，
// usage 落库时回写消耗。根 Key（旧 api_key）不注入 context，不受任何限制。

type gwKeyCtxType int

const gwKeyCtxKey gwKeyCtxType = 1

func contextWithGatewayKey(ctx context.Context, k *GatewayKey) context.Context {
	return context.WithValue(ctx, gwKeyCtxKey, k)
}

func gatewayKeyFromCtx(ctx context.Context) *GatewayKey {
	k, _ := ctx.Value(gwKeyCtxKey).(*GatewayKey)
	return k
}

// HashGatewayKey sha256 hex 摘要（明文不落库、不参与比对）
func HashGatewayKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// resolveGatewayKey 中间件解析：返回 nil,nil = 根 Key（或未配置命名 Key 场景直接放行）；
// 非 nil errResp = 认证/限流失败，已构造好响应。
// RPM 命中时 errResp.status = 429，调用方应带 Retry-After 回写。
func (am *AuthManager) resolveGatewayKey(key string) (*GatewayKey, *errorResponse) {
	if am.db == nil || key == "" {
		return nil, &errorResponse{status: http.StatusUnauthorized, msg: "invalid API key"}
	}
	// 根 Key 优先（常数时间比较，规避计时侧信道）
	if am.ValidateAPIKey(key) {
		return nil, nil
	}
	k, err := am.db.GetGatewayKeyByHash(HashGatewayKey(key))
	if err != nil {
		return nil, &errorResponse{status: http.StatusInternalServerError, msg: err.Error()}
	}
	if k == nil {
		return nil, &errorResponse{status: http.StatusUnauthorized, msg: "invalid API key"}
	}
	if !k.Enabled {
		return nil, &errorResponse{status: http.StatusForbidden, msg: "gateway key is disabled: " + k.Name}
	}
	if k.RPMLimit > 0 && !am.gwRPM.allow(k.ID, k.RPMLimit) {
		return nil, &errorResponse{status: http.StatusTooManyRequests,
			msg: "gateway key rate limited (" + strconv.Itoa(k.RPMLimit) + " rpm): " + k.Name}
	}
	return k, nil
}

// checkGatewayKeyRequest 转发前拦截：配额耗尽 / 模型不在白名单。
// model 须已去掉 provider 前缀并小写（与白名单同规范）。
func checkGatewayKeyRequest(k *GatewayKey, model string) *errorResponse {
	if k == nil {
		return nil
	}
	if k.QuotaTotal > 0 && k.QuotaUsed >= k.QuotaTotal {
		return &errorResponse{status: http.StatusTooManyRequests,
			msg: "gateway key token quota exhausted (" + strconv.FormatInt(k.QuotaUsed, 10) + "/" +
				strconv.FormatInt(k.QuotaTotal, 10) + "): " + k.Name}
	}
	if !k.modelAllowed(model) {
		return &errorResponse{status: http.StatusForbidden,
			msg: "model not allowed for this gateway key: " + model}
	}
	return nil
}

// gwRPM 命名 Key 的进程内滑动窗口限速（60s 窗口，与转发无关的独立锁）
type gwRPMTracker struct {
	mu    sync.Mutex
	hits  map[int64][]time.Time
	prune time.Time
}

const gwRPMWindow = time.Minute

func (t *gwRPMTracker) allow(id int64, limit int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.hits == nil {
		t.hits = map[int64][]time.Time{}
	}
	now := time.Now()
	// 容量兜底：Key 数量无界增长不现实，但陈旧窗口必须淘汰
	if len(t.hits) >= 4096 && now.Sub(t.prune) > gwRPMWindow {
		for kid, ts := range t.hits {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > gwRPMWindow {
				delete(t.hits, kid)
			}
		}
		t.prune = now
	}
	cutoff := now.Add(-gwRPMWindow)
	live := t.hits[id][:0]
	for _, ts := range t.hits[id] {
		if ts.After(cutoff) {
			live = append(live, ts)
		}
	}
	if len(live) >= limit {
		t.hits[id] = live
		return false
	}
	t.hits[id] = append(live, now)
	return true
}
