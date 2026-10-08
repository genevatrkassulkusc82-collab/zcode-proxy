package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// convertZeroEventUpstream 假上游：只发 keepalive 注释后干净关闭（零事件干净 EOF）
func convertZeroEventUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": keepalive\n\n"))
	}))
}

// convertAbortUpstream 假上游：发一帧注释后中途异常断开（客户端读到非 EOF 读错误）
func convertAbortUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": keepalive\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
}

// convertCanceledReq 上下文已取消的请求（模拟客户端先行断开）
func convertCanceledReq() *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
}

// convertRecordedStatus 取该测试 DB 落库的唯一一条用量记录状态码
func convertRecordedStatus(t *testing.T, db *DB) int {
	t.Helper()
	recs, err := db.ListUsageRecords(10)
	if err != nil {
		t.Fatalf("ListUsageRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("usage records = %d, want 1", len(recs))
	}
	return recs[0].StatusCode
}

// TestStreamOpenAIZeroEventsCleanEOF 零事件干净 EOF 不得合成 200 空助手回合
// （与聚合路径同规：应发 error chunk + [DONE] 并按 502 落库）
func TestStreamOpenAIZeroEventsCleanEOF(t *testing.T) {
	srv := convertZeroEventUpstream(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()

	db := newCompletionsTestDB(t)
	z := &ZCodeAPI{db: db}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	z.streamOpenAI(w, nil, resp, "my-model", false, &Account{}, req, []byte(`{}`), time.Now())

	out := w.Body.String()
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "upstream stream ended without any events") {
		t.Fatalf("zero-event clean EOF must emit error frame, got: %q", out)
	}
	if strings.Contains(out, `"finish_reason"`) {
		t.Fatalf("zero-event clean EOF must not synthesize a successful turn: %q", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("must end with [DONE]: %q", out)
	}
	if got := convertRecordedStatus(t, db); got != 502 {
		t.Fatalf("recorded status = %d, want 502", got)
	}
}

// TestStreamCompletionsZeroEventsCleanEOF legacy 端点同规：error 帧 + [DONE]，无 finish 帧
func TestStreamCompletionsZeroEventsCleanEOF(t *testing.T) {
	srv := convertZeroEventUpstream(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()

	db := newCompletionsTestDB(t)
	z := &ZCodeAPI{db: db}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", nil)
	z.streamCompletions(w, nil, resp, "my-model", false, false, "",
		&Account{}, req, []byte(`{}`), time.Now())

	out := w.Body.String()
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "upstream stream ended without any events") {
		t.Fatalf("zero-event clean EOF must emit error frame, got: %q", out)
	}
	if strings.Contains(out, `"finish_reason"`) {
		t.Fatalf("zero-event clean EOF must not synthesize a successful turn: %q", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("must end with [DONE]: %q", out)
	}
	if got := convertRecordedStatus(t, db); got != 502 {
		t.Fatalf("recorded status = %d, want 502", got)
	}
}

// TestStreamResponsesZeroEventsCleanEOF Responses 端点同规：response.failed 而非 completed
func TestStreamResponsesZeroEventsCleanEOF(t *testing.T) {
	srv := convertZeroEventUpstream(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()

	db := newCompletionsTestDB(t)
	z := &ZCodeAPI{db: db}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	z.streamResponses(w, nil, resp, "my-model", &Account{}, req, []byte(`{}`), time.Now())

	out := w.Body.String()
	if !strings.Contains(out, "event: response.failed") || !strings.Contains(out, "upstream stream ended without any events") {
		t.Fatalf("zero-event clean EOF must emit response.failed, got: %q", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("zero-event clean EOF must not synthesize a completed response: %q", out)
	}
	if got := convertRecordedStatus(t, db); got != 502 {
		t.Fatalf("recorded status = %d, want 502", got)
	}
}

// TestStreamOpenAIMessageStartOnlySucceeds 只有 message_start 的合法流：聚合路径判成功，
// 流式路径同样不得误报（一致性护栏）
func TestStreamOpenAIMessageStartOnlySucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	z.streamOpenAI(w, nil, resp, "my-model", false, &Account{}, req, []byte(`{}`), time.Now())

	out := w.Body.String()
	if strings.Contains(out, `"error"`) {
		t.Fatalf("message_start-only stream must not fail, got: %q", out)
	}
	if !strings.Contains(out, `"finish_reason"`) || !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("message_start-only stream must complete normally, got: %q", out)
	}
	if got := convertRecordedStatus(t, db); got != 200 {
		t.Fatalf("recorded status = %d, want 200", got)
	}
}

// TestStreamClientCancelRecordedAs499 客户端主动断开的中途读错误按 499 落库
// （nginx 语义 client closed request），不得计入上游 502
func TestStreamClientCancelRecordedAs499(t *testing.T) {
	t.Run("passthrough", func(t *testing.T) {
		srv := convertAbortUpstream(t)
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("upstream get: %v", err)
		}
		defer resp.Body.Close()

		db := newCompletionsTestDB(t)
		z := &ZCodeAPI{db: db}
		w := httptest.NewRecorder()
		rc := &relayCtx{proto: protocolAnthropic, clientStream: true, clientModel: "my-model"}
		streamProtocolResponse(w, rc, resp, &Account{}, convertCanceledReq(), []byte(`{}`), z, time.Now())
		if got := convertRecordedStatus(t, db); got != 499 {
			t.Fatalf("client cancel recorded as %d, want 499", got)
		}
	})

	t.Run("openai", func(t *testing.T) {
		srv := convertAbortUpstream(t)
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("upstream get: %v", err)
		}
		defer resp.Body.Close()

		db := newCompletionsTestDB(t)
		z := &ZCodeAPI{db: db}
		w := httptest.NewRecorder()
		z.streamOpenAI(w, nil, resp, "my-model", false, &Account{}, convertCanceledReq(), []byte(`{}`), time.Now())
		if got := convertRecordedStatus(t, db); got != 499 {
			t.Fatalf("client cancel recorded as %d, want 499", got)
		}
	})

	t.Run("completions", func(t *testing.T) {
		srv := convertAbortUpstream(t)
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("upstream get: %v", err)
		}
		defer resp.Body.Close()

		db := newCompletionsTestDB(t)
		z := &ZCodeAPI{db: db}
		w := httptest.NewRecorder()
		z.streamCompletions(w, nil, resp, "my-model", false, false, "",
			&Account{}, convertCanceledReq(), []byte(`{}`), time.Now())
		if got := convertRecordedStatus(t, db); got != 499 {
			t.Fatalf("client cancel recorded as %d, want 499", got)
		}
	})

	t.Run("responses", func(t *testing.T) {
		srv := convertAbortUpstream(t)
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("upstream get: %v", err)
		}
		defer resp.Body.Close()

		db := newCompletionsTestDB(t)
		z := &ZCodeAPI{db: db}
		w := httptest.NewRecorder()
		z.streamResponses(w, nil, resp, "my-model", &Account{}, convertCanceledReq(), []byte(`{}`), time.Now())
		if got := convertRecordedStatus(t, db); got != 499 {
			t.Fatalf("client cancel recorded as %d, want 499", got)
		}
	})
}

// TestStreamUpstreamInterruptAliveClientRecordedAs502 对照组：同样中途断流但客户端
// 仍在线（非客户端取消），必须仍按 502 落库
func TestStreamUpstreamInterruptAliveClientRecordedAs502(t *testing.T) {
	srv := convertAbortUpstream(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()

	db := newCompletionsTestDB(t)
	z := &ZCodeAPI{db: db}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	z.streamOpenAI(w, nil, resp, "my-model", false, &Account{}, req, []byte(`{}`), time.Now())

	out := w.Body.String()
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "upstream stream interrupted") {
		t.Fatalf("mid-stream abort must emit interrupted error frame, got: %q", out)
	}
	if got := convertRecordedStatus(t, db); got != 502 {
		t.Fatalf("upstream interrupt with live client recorded as %d, want 502", got)
	}
}
