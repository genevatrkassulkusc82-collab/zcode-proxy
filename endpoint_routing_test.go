package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func routingTestServer(t *testing.T, code int, body string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if r.URL.Path != routingConfigPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
}

func TestRoutingKeyNormalizesURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages", "https://zcode.z.ai:443/api/v1/zcode-plan/anthropic/v1/messages"},
		{"https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages/", "https://zcode.z.ai:443/api/v1/zcode-plan/anthropic/v1/messages"},
		{"http://api.z.ai/api/anthropic/v1/messages", "http://api.z.ai:80/api/anthropic/v1/messages"},
		{"https://api.z.ai:8443/x", "https://api.z.ai:8443/x"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.in)
		if err != nil {
			t.Fatalf("parse %s: %v", c.in, err)
		}
		if got := routingKey(u); got != c.want {
			t.Fatalf("routingKey(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestRoutingResolveRewritesAndPreservesQuery(t *testing.T) {
	var hits atomic.Int32
	srv := routingTestServer(t, 200, `{"code":0,"data":{"proxyEndpoint":{"mapping":[
		{"from":"https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages","to":"https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages"}
	]}}}`, &hits)
	defer srv.Close()

	r := NewEndpointRouter(srv.URL)
	r.testTimeout = 2 * time.Second
	got := r.Resolve("https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages?app_version=3.14.4")
	if got != "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages?app_version=3.14.4" {
		t.Fatalf("Resolve rewrote to %s", got)
	}
	// TTL 内不再刷新
	r.Resolve("https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages")
	if n := hits.Load(); n != 1 {
		t.Fatalf("expected 1 config fetch within TTL, got %d", n)
	}
}

func TestRoutingFailOpen(t *testing.T) {
	var hits atomic.Int32
	srv := routingTestServer(t, 200, `{"code":500,"msg":"boom"}`, &hits)
	defer srv.Close()

	r := NewEndpointRouter(srv.URL)
	r.testTimeout = 2 * time.Second
	original := "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages"
	if got := r.Resolve(original); got != original {
		t.Fatalf("fail-open: Resolve returned %s", got)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("expected exactly 1 fetch before cooldown, got %d", n)
	}
	// 冷却期内不再尝试
	r.Resolve(original)
	if n := hits.Load(); n != 1 {
		t.Fatalf("cooldown not honored, fetches=%d", n)
	}
}

func TestRoutingRejectsNonPlainHTTPSMapping(t *testing.T) {
	for _, bad := range []string{"http://evil.example/x", "https://h/x?a=1", "https://u:p@h/x", "not a url"} {
		if _, err := parsePlainURL(bad); err == nil {
			t.Fatalf("parsePlainURL(%q) should fail", bad)
		}
	}
	if _, err := parsePlainURL("https://ok.example/x"); err != nil {
		t.Fatalf("plain https URL should pass: %v", err)
	}
}
