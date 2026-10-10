package main

import (
	"testing"
	"time"
)

func TestNormalizeModelWhitelist(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"GLM-5.3", "glm-5.3"},
		{" GLM-5.3 , glm-5.2 ,,GLM-5.3 ", "glm-5.3,glm-5.2"},
		{",,", ""},
	}
	for _, c := range cases {
		if got := normalizeModelWhitelist(c.in); got != c.want {
			t.Errorf("normalizeModelWhitelist(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGatewayKeyModelAllowed(t *testing.T) {
	k := &GatewayKey{Models: "glm-5.3,glm-5.2"}
	if !k.modelAllowed("glm-5.3") {
		t.Error("whitelisted model should pass")
	}
	if k.modelAllowed("glm-4.6") {
		t.Error("non-whitelisted model should be rejected")
	}
	open := &GatewayKey{}
	if !open.modelAllowed("anything") {
		t.Error("empty whitelist should allow all")
	}
}

func TestGatewayKeyRequestChecks(t *testing.T) {
	// 配额未超限 + 白名单命中 → nil
	k := &GatewayKey{QuotaTotal: 100, QuotaUsed: 99, Models: "glm-5.3"}
	if errResp := checkGatewayKeyRequest(k, "glm-5.3"); errResp != nil {
		t.Errorf("expected pass under quota, got %v", errResp.msg)
	}
	// 配额用尽 → 429
	k.QuotaUsed = 100
	if errResp := checkGatewayKeyRequest(k, "glm-5.3"); errResp == nil || errResp.status != 429 {
		t.Errorf("expected 429 quota exhausted, got %+v", errResp)
	}
	// 白名单外 → 403
	k.QuotaUsed = 0
	if errResp := checkGatewayKeyRequest(k, "glm-4.6"); errResp == nil || errResp.status != 403 {
		t.Errorf("expected 403 model not allowed, got %+v", errResp)
	}
	// nil Key（根 Key）恒放行
	if errResp := checkGatewayKeyRequest(nil, "glm-4.6"); errResp != nil {
		t.Error("nil gateway key (root) must bypass checks")
	}
}

func TestGatewayRPMTracker(t *testing.T) {
	var tr gwRPMTracker
	if !tr.allow(1, 3) || !tr.allow(1, 3) || !tr.allow(1, 3) {
		t.Fatal("first 3 hits should pass")
	}
	if tr.allow(1, 3) {
		t.Error("4th hit within window should be rejected")
	}
	// 其他 Key 不受影响
	if !tr.allow(2, 3) {
		t.Error("separate key should have its own window")
	}
	// 窗口过期后恢复（直接改时间戳模拟，避免真实等待）
	tr.mu.Lock()
	tr.hits[1] = []time.Time{time.Now().Add(-2 * time.Minute)}
	tr.mu.Unlock()
	if !tr.allow(1, 3) {
		t.Error("expired window entries should not count")
	}
}

func TestHashGatewayKey(t *testing.T) {
	h1 := HashGatewayKey("sk-abc")
	h2 := HashGatewayKey("sk-abc")
	h3 := HashGatewayKey("sk-abd")
	if h1 != h2 || h1 == h3 || len(h1) != 64 {
		t.Error("hash must be deterministic sha256 hex")
	}
}
