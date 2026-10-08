package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOffTicketFrom(t *testing.T) {
	tk, err := offTicketFrom(map[string]interface{}{
		"ticket_id": "t-123", "state": "queued", "position": float64(4), "active_deadline": float64(1790681000),
	})
	if err != nil {
		t.Fatalf("offTicketFrom: %v", err)
	}
	if tk.ID != "t-123" || tk.State != "queued" || tk.Position != 4 || tk.ActiveDeadline != 1790681000 {
		t.Fatalf("bad ticket: %+v", tk)
	}
	if _, err := offTicketFrom(map[string]interface{}{"state": "queued"}); err == nil {
		t.Fatal("missing ticket_id should fail")
	}
}

func TestOffPeakStatePredicates(t *testing.T) {
	if !offPeakStateReady("ready") || !offPeakStateReady("active") {
		t.Fatal("ready states")
	}
	if offPeakStateReady("queued") {
		t.Fatal("queued is not ready")
	}
	if !offPeakStateExpired("expired") || !offPeakStateExpired("not_found") {
		t.Fatal("expired states")
	}
	if !offPeakStateTerminal("settled") || offPeakStateTerminal("queued") {
		t.Fatal("terminal states")
	}
}

func TestUsageSniffReader(t *testing.T) {
	sse := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":120,"output_tokens":1}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","usage":{"output_tokens":77,"input_tokens":130},"delta":{"stop_reason":"end_turn"}}`,
		"",
	}, "\n")
	r := newUsageSniffReader(strings.NewReader(sse))
	out, err := ioReadAll(r)
	if err != nil && err.Error() != "EOF" {
		t.Fatalf("read: %v", err)
	}
	if len(out) != len(sse) {
		t.Fatalf("passthrough length mismatch: %d != %d", len(out), len(sse))
	}
	u := r.usage()
	if u == nil {
		t.Fatal("expected usage sniffed")
	}
	if u.InputTokens != 130 || u.OutputTokens != 77 || u.StopReason != "end_turn" {
		t.Fatalf("bad usage: %+v", u)
	}
}

func TestWriteSSEErrorEvent(t *testing.T) {
	w := httptest.NewRecorder()
	writeSSEErrorEvent(w, "boom \"quoted\"")
	body := w.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, `"message":"boom \"quoted\""`) {
		t.Fatalf("bad SSE error frame: %s", body)
	}
}

func TestOffPeakWait(t *testing.T) {
	keeps := 0
	start := time.Now()
	if !offPeakWait(context.Background(), 60*time.Millisecond, time.Hour, func() { keeps++ }, true) {
		t.Fatal("should complete")
	}
	if keeps != 0 || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("keepalive fired early or sleep skipped: keeps=%d elapsed=%v", keeps, time.Since(start))
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if offPeakWait(ctx, 10*time.Second, time.Hour, func() {}, true) {
		t.Fatal("ctx cancel should return false")
	}
}

// ioReadAll 避免直接依赖 io 包名冲突的小封装
func ioReadAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	p := make([]byte, 4096)
	for {
		n, err := r.Read(p)
		buf = append(buf, p[:n]...)
		if err != nil {
			return buf, err
		}
	}
}
