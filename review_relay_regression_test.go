package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReviewBalancedFallbackHonorsPaidGuards(t *testing.T) {
	for _, rejection := range []struct{ name, body string }{
		{"risk", `{"error":{"code":3012,"message":"unusual activity"}}`},
		{"captcha", `{"error":{"message":"captcha required"}}`},
	} {
		for _, guard := range []string{"disabled", "cooling", "daily_cap", "eligible"} {
			t.Run(rejection.name+"/"+guard, func(t *testing.T) {
				p, db := newPaidTestPool(t)
				for key, value := range map[string]string{"paid_fallback_mode": "balanced", "captcha_mode": "off"} {
					if err := db.SetSetting(key, value); err != nil {
						t.Fatal(err)
					}
				}
				a := mkDualAccount("review-balanced", StatusActive)
				id, err := db.UpsertAccount(a)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.UpdateAccountFieldsWithPriority(id, "", "", true, guard != "disabled", nil); err != nil {
					t.Fatal(err)
				}
				if guard == "cooling" {
					if err := db.SetAccountPaidStatus(id, "test cooldown", time.Now().Add(time.Hour).Unix()); err != nil {
						t.Fatal(err)
					}
				}
				if guard == "daily_cap" {
					if err := db.SetSetting("paid_daily_token_cap", "1"); err != nil {
						t.Fatal(err)
					}
					if err := db.InsertUsageRecord(&UsageRecord{AccountID: id, Channel: ChannelPaid, TotalTokens: 1}); err != nil {
						t.Fatal(err)
					}
					if used, err := db.PaidTokensToday(); err != nil || used != 1 {
						t.Fatalf("daily cap fixture: used=%d err=%v", used, err)
					}
				}
				var free, paid atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/free":
						free.Add(1)
						w.WriteHeader(http.StatusForbidden)
						io.WriteString(w, rejection.body)
					case "/paid":
						paid.Add(1)
						// Terminal error proves dispatch without spawning success quota refreshes.
						w.WriteHeader(http.StatusTeapot)
						io.WriteString(w, `{"error":{"message":"local paid endpoint reached"}}`)
					default:
						t.Errorf("unexpected local path %q", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				cfg := &FileConfig{Upstream: UpstreamURLs{Zai: server.URL + "/free", ZaiFallback: server.URL + "/paid", Bigmodel: server.URL + "/free"}}
				z := &ZCodeAPI{cfg: cfg, db: db, pool: p, egress: NewEgressProxy(db),
					captcha: NewCaptchaService(cfg, db, "3.14.4"),
					routing: &EndpointRouter{snapshot: &routingSnapshot{expiresAt: time.Now().Add(time.Hour)}},
				}
				w := httptest.NewRecorder()
				z.relay(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), &relayCtx{
					provider: "zai", proto: protocolAnthropic,
					body: map[string]interface{}{"model": "GLM-5.3", "messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}}},
				})
				if free.Load() != 1 {
					t.Fatalf("free dispatches = %d, want 1; response=%s", free.Load(), w.Body.String())
				}
				wantPaid, wantStatus := int32(0), http.StatusServiceUnavailable
				if guard == "eligible" {
					wantPaid, wantStatus = 1, http.StatusTeapot
				}
				if paid.Load() != wantPaid {
					t.Errorf("paid dispatches = %d, want %d (%s)", paid.Load(), wantPaid, guard)
				}
				if w.Code != wantStatus {
					t.Errorf("HTTP status = %d, want %d; response=%s", w.Code, wantStatus, w.Body.String())
				}
			})
		}
	}
}

func TestReviewOffPeakKnownStates(t *testing.T) {
	for _, state := range []string{"queued", "ready", "active", "settled", "expired", "not_found", "", "unexpected"} {
		t.Run(state, func(t *testing.T) {
			want := state != "" && state != "unexpected"
			if got := offPeakStateKnown(state); got != want {
				t.Fatalf("offPeakStateKnown(%q) = %v, want %v", state, got, want)
			}
		})
	}
}

func TestReviewResponsesToolArgumentsAtEOF(t *testing.T) {
	for _, tc := range []struct{ name, delta, ending, terminal string }{
		{"partial_eof", `{"a":`, "", "response.failed"},
		{"valid_delta", `{"a":1}`, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "response.completed"},
		{"start_arguments", "", "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "response.completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db := newPaidTestPool(t)
			stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{}}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_review\",\"name\":\"lookup\",\"input\":{\"a\":1}}}\n\n"
			if tc.delta != "" {
				stream += fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%q}}\n\n", tc.delta)
			}
			stream += tc.ending
			w := httptest.NewRecorder()
			z := &ZCodeAPI{db: db}
			z.streamResponses(w, w, &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}, "GLM-5.3",
				mkDualAccount("review-stream", StatusActive), httptest.NewRequest(http.MethodPost, "/v1/responses", nil), []byte(`{"model":"GLM-5.3"}`), time.Now())
			output := w.Body.String()
			for _, event := range []string{"response.failed", "response.completed"} {
				want := 0
				if event == tc.terminal {
					want = 1
				}
				if got := strings.Count(output, "event: "+event+"\n"); got != want {
					t.Errorf("%s count = %d, want %d; output=%s", event, got, want, output)
				}
			}
		})
	}
}

func TestReviewAccountSlotResizePreservesOccupancy(t *testing.T) {
	for _, tc := range []struct{ name, initial, next string }{{"grow", "1", "2"}, {"shrink", "2", "1"}} {
		t.Run(tc.name, func(t *testing.T) {
			p, db := newPaidTestPool(t)
			a := &Account{ID: 41}
			setCap := func(value string) {
				t.Helper()
				if err := db.SetSetting("max_concurrent_per_account", value); err != nil {
					t.Fatal(err)
				}
			}
			acquire := func(channel string, want bool) func() {
				t.Helper()
				release, ok := p.AcquireAccountSlot(context.Background(), a, channel, 0)
				if ok {
					t.Cleanup(release)
				}
				if ok != want {
					t.Fatalf("AcquireAccountSlot(%s) = %v, want %v with old holders active", channel, ok, want)
				}
				return release
			}
			setCap(tc.initial)
			first := acquire(ChannelFree, true)
			var second func()
			if tc.name == "shrink" {
				second = acquire(ChannelFree, true)
			}
			setCap(tc.next)
			if tc.name == "grow" {
				second = acquire(ChannelFree, true)
			}
			acquire(ChannelFree, false)
			paid := acquire(ChannelPaid, true) // Channels retain independent occupancy.
			paid()
			first()
			first() // Idempotent release must not discard another holder.
			if tc.name == "shrink" {
				acquire(ChannelFree, false)
			}
			second()
			acquire(ChannelFree, true)
		})
	}
}
