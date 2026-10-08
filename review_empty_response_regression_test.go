package main

// 空响应防线回归（2026-10-08 生产事故）：
// 1. 上游 2xx "成功空响应"（content 为零内容的 message 信封 / message_start→
//    message_stop 空流）不得被洗成 200 空助手回合——非流式在写头前换路径，
//    流式补错误帧、用量按 502 落库。
// 2. 付费通道事件（秒级 429 冷却）不得覆盖免费侧 last_error——否则 503 提示
//    拿付费理由解释免费 24h 风控冷却，误导排障方向。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// isEmptyMessageBody 判定表：零内容 message 信封为真；有内容块/错误信封为假
func TestIsEmptyMessageBody(t *testing.T) {
	cases := []struct {
		name, body string
		want       bool
	}{
		{"empty array", `{"type":"message","content":[],"stop_reason":"end_turn"}`, true},
		{"missing content", `{"type":"message","stop_reason":"end_turn"}`, true},
		{"empty string", `{"type":"message","content":""}`, true},
		{"no type empty content", `{"content":[]}`, true},
		{"text block", `{"type":"message","content":[{"type":"text","text":"hi"}]}`, false},
		{"thinking block", `{"type":"message","content":[{"type":"thinking","thinking":"hm"}]}`, false},
		{"error envelope", `{"type":"error","error":{"message":"captcha"}}`, false},
		{"non-object", `"ok"`, false},
	}
	for _, c := range cases {
		if got := isEmptyMessageBody([]byte(c.body)); got != c.want {
			t.Errorf("%s: isEmptyMessageBody = %v, want %v", c.name, got, c.want)
		}
	}
}

// 非流式：免费+付费全部返回 2xx 空内容 message → 不得把空 200 写回客户端，
// 终态 503 且失败明细含"空内容"
func TestEmptyNonStream200FailsOver(t *testing.T) {
	p, db := newPaidTestPool(t)
	for key, value := range map[string]string{"paid_fallback_mode": "balanced", "captcha_mode": "off"} {
		if err := db.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	a := mkDualAccount("empty-200", StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAccountFieldsWithPriority(id, "", "", true, true, nil); err != nil {
		t.Fatal(err)
	}
	emptyBody := `{"type":"message","content":[],"stop_reason":"end_turn"}`
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, emptyBody)
	}))
	defer srv.Close()
	cfg := &FileConfig{Upstream: UpstreamURLs{Zai: srv.URL, ZaiFallback: srv.URL, Bigmodel: srv.URL}}
	z := &ZCodeAPI{cfg: cfg, db: db, pool: p, egress: NewEgressProxy(db),
		captcha: NewCaptchaService(cfg, db, "3.14.4"),
		routing: &EndpointRouter{snapshot: &routingSnapshot{expiresAt: time.Now().Add(time.Hour)}},
	}
	w := httptest.NewRecorder()
	z.relay(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), &relayCtx{
		provider: "zai", proto: protocolAnthropic,
		body: map[string]interface{}{"model": "GLM-5.3", "messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}}},
	})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (empty 200 must not reach client)", w.Code)
	}
	out := w.Body.String()
	if strings.Contains(out, `"content":[]`) {
		t.Fatalf("empty content leaked to client: %s", out)
	}
	if !strings.Contains(out, "空内容") {
		t.Fatalf("failure detail must name the empty-content cause: %s", out)
	}
	if hits == 0 {
		t.Fatal("upstream never hit")
	}
}

// 有内容块的 200 不受空响应防线影响（防误伤）
func TestNonStream200WithContentStillPasses(t *testing.T) {
	p, db := newPaidTestPool(t)
	for key, value := range map[string]string{"paid_fallback_mode": "balanced", "captcha_mode": "off"} {
		if err := db.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	a := mkDualAccount("ok-200", StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAccountFieldsWithPriority(id, "", "", true, true, nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()
	cfg := &FileConfig{Upstream: UpstreamURLs{Zai: srv.URL, ZaiFallback: srv.URL, Bigmodel: srv.URL}}
	z := &ZCodeAPI{cfg: cfg, db: db, pool: p, egress: NewEgressProxy(db),
		captcha: NewCaptchaService(cfg, db, "3.14.4"),
		routing: &EndpointRouter{snapshot: &routingSnapshot{expiresAt: time.Now().Add(time.Hour)}},
	}
	w := httptest.NewRecorder()
	z.relay(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), &relayCtx{
		provider: "zai", proto: protocolAnthropic,
		body: map[string]interface{}{"model": "GLM-5.3", "messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"text":"hi"`) {
		t.Fatalf("content missing: %s", w.Body.String())
	}
}

// Anthropic 透传流：message_start→message_stop 空流补错误帧
func TestPassthroughEmptyStreamGetsErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()
	db := newCompletionsTestDB(t)
	z := &ZCodeAPI{db: db}
	w := httptest.NewRecorder()
	rc := &relayCtx{proto: protocolAnthropic, clientStream: true, clientModel: "GLM-5.3"}
	streamProtocolResponse(w, rc, resp, &Account{}, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), []byte(`{}`), z, time.Now())
	out := w.Body.String()
	if !strings.Contains(out, "empty response") {
		t.Fatalf("expected error event for empty stream, got: %q", out)
	}
}

// OpenAI 转换流（OmniRoute 走的路径）：空流发错误 chunk + [DONE]，不合成空助手回合
func TestStreamOpenAIEmptyStreamGetsErrorChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()
	db := newCompletionsTestDB(t)
	z := &ZCodeAPI{db: db}
	w := httptest.NewRecorder()
	z.streamOpenAI(w, w, resp, "GLM-5.3", false, &Account{}, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), []byte(`{}`), time.Now())
	out := w.Body.String()
	if !strings.Contains(out, "without any content") {
		t.Fatalf("expected no-content error chunk, got: %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("missing [DONE] terminator: %q", out)
	}
}

// OpenAI 聚合路径（客户端非流式 + 上游 SSE 空流）：502 而非空 choices 200
func TestAggregateEmptyStreamOpenAIGets502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()
	db := newCompletionsTestDB(t)
	z := &ZCodeAPI{db: db}
	w := httptest.NewRecorder()
	streamProtocolResponse(w, &relayCtx{proto: protocolOpenAI, clientStream: false, clientModel: "GLM-5.3"},
		resp, &Account{}, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), []byte(`{}`), z, time.Now())
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "without any content") {
		t.Fatalf("body: %s", w.Body.String())
	}
}

// 付费通道冷却不覆盖免费侧原因：风控 24h 冷却后再吃付费 429，
// CoolingInfo 必须仍报风控原因（503 提示与面板据此解释免费冷却）
func TestPaidCooldownDoesNotClobberFreeReason(t *testing.T) {
	p, db := newPaidTestPool(t)
	a := mkDualAccount("reason-sep", StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := db.GetAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	p.MarkRiskCooling(fresh, "免费通道风控拦截（unusual activity）")
	p.MarkPaidCooling(fresh, "上游限流（HTTP 429，model=GLM-5.3）, 冷却", 30)

	if fresh.LastError != "免费通道风控拦截（unusual activity）" {
		t.Fatalf("free LastError clobbered: %q", fresh.LastError)
	}
	if fresh.PaidLastError != "上游限流（HTTP 429，model=GLM-5.3）, 冷却" {
		t.Fatalf("PaidLastError = %q", fresh.PaidLastError)
	}
	until, reason := p.CoolingInfo("zai", "", nil)
	if until <= time.Now().Unix() {
		t.Fatalf("free cooling until = %d, want 24h-scale", until)
	}
	if !strings.Contains(reason, "风控拦截") {
		t.Fatalf("CoolingInfo reason = %q, want free-side risk reason", reason)
	}

	// DB 侧同样分离：SetAccountPaidStatus 不得再写 last_error 列
	dbFresh, err := db.GetAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dbFresh.LastError, "风控拦截") {
		t.Fatalf("DB last_error clobbered by paid write: %q", dbFresh.LastError)
	}
	if dbFresh.PaidLastError == "" {
		t.Fatal("DB paid_last_error missing")
	}
}
