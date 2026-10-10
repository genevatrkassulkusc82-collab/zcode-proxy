package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 凭证类 header 不得透传上游：官方客户端把 x-coding-plan-api-key 当 off-peak 首选
// apiKey、x-bigmodel-authorization 是其 reserved-auth 的 codingPlanAuthorization——
// 透传会让上游把请求认到客户端账号（用量/计费错位），被拒时误判到池账号。
// 非凭证的标注/元数据头（query-source、高速卡元数据）仍照常透传。
func TestBuildUpstreamRequestStripsClientCredentials(t *testing.T) {
	db := newCompletionsTestDB(t)
	z := &ZCodeAPI{
		db:      db,
		routing: NewEndpointRouter(""),
		cfg: &FileConfig{Upstream: UpstreamURLs{
			Zai:         "https://up.test/api/v1/zcode-plan/anthropic/v1/messages",
			ZaiFallback: "https://fallback.test/api/v1/messages",
			Bigmodel:    "https://bm.test/api/v1/messages",
		}},
	}
	a := &Account{Provider: "zai", ZCodeJWT: "pool-jwt-123", DeviceMid: "mid-1"}

	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	r.Header.Set("X-Coding-Plan-Api-Key", "client-project-pat")
	r.Header.Set("X-Bigmodel-Authorization", "Bearer client-jwt")
	r.Header.Set("X-Highspeed-Card-ID", "card-9")
	r.Header.Set("X-ZCode-Query-Source", "desktop")
	r.Header.Set("Anthropic-Beta", "fast-mode-2026-02-01")

	_, headers := z.buildUpstreamRequest(a, "", "", false, r, true)

	// 池凭证注入（客户端 Authorization 被替换，而非透传）
	if got := headers["Authorization"]; got != "Bearer pool-jwt-123" {
		t.Fatalf("Authorization = %q, want pool credential", got)
	}
	// 凭证头绝不透传
	for _, k := range []string{"x-coding-plan-api-key", "x-bigmodel-authorization"} {
		if v, ok := headers[k]; ok {
			t.Fatalf("credential header %s leaked upstream: %q", k, v)
		}
	}
	// 非凭证头照常透传
	want := map[string]string{
		"x-highspeed-card-id":  "card-9",
		"x-zcode-query-source": "desktop",
		"anthropic-beta":       "fast-mode-2026-02-01",
	}
	for k, v := range want {
		if headers[k] != v {
			t.Fatalf("%s = %q, want %q", k, headers[k], v)
		}
	}
}
