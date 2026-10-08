package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A terminal local response proves dispatch without starting quota refreshes.
func reviewAdmissionAPI(t *testing.T, p *AccountPool, db *DB) (*ZCodeAPI, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, `{"error":{"message":"local paid endpoint reached"}}`)
	}))
	t.Cleanup(server.Close)
	cfg := &FileConfig{Upstream: UpstreamURLs{Zai: server.URL, ZaiFallback: server.URL, Bigmodel: server.URL}}
	return &ZCodeAPI{cfg: cfg, db: db, pool: p, egress: NewEgressProxy(db),
		captcha: NewCaptchaService(cfg, db, "3.14.4"),
		routing: &EndpointRouter{snapshot: &routingSnapshot{expiresAt: time.Now().Add(time.Hour)}},
	}, calls
}

func TestReviewAPIKeyOnlyNeverHonorsPaidCooldown(t *testing.T) {
	for _, cooling := range []bool{false, true} {
		name := "eligible_with_fallback_disabled"
		if cooling {
			name = "paid_cooling_with_fallback_disabled"
		}
		t.Run(name, func(t *testing.T) {
			p, db := newPaidTestPool(t)
			if err := db.SetSetting("paid_fallback_mode", PaidModeNever); err != nil {
				t.Fatal(err)
			}
			a := mkDualAccount("review-keyonly", StatusActive)
			a.ZCodeJWT = ""
			a.AuthType = "apikey"
			id, err := db.UpsertAccount(a)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.UpdateAccountFieldsWithPriority(id, "", "", true, false, nil); err != nil {
				t.Fatal(err)
			}
			if cooling {
				if err := db.SetAccountPaidStatus(id, "test cooldown", time.Now().Add(time.Hour).Unix()); err != nil {
					t.Fatal(err)
				}
			}
			z, calls := reviewAdmissionAPI(t, p, db)
			w := httptest.NewRecorder()
			z.relay(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), &relayCtx{
				provider: "zai", proto: protocolAnthropic,
				body: map[string]interface{}{"model": "GLM-5.3", "messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}}},
			})
			want := int32(1)
			if cooling {
				want = 0
			}
			if got := calls.Load(); got != want {
				t.Fatalf("paid dispatches = %d, want %d; response=%s", got, want, w.Body.String())
			}
		})
	}
}

func TestReviewForwardOnceReloadsPaidEligibility(t *testing.T) {
	for _, guard := range []string{"eligible", "cooling", "fallback_disabled"} {
		t.Run(guard, func(t *testing.T) {
			p, db := newPaidTestPool(t)
			id, err := db.UpsertAccount(mkDualAccount("review-stale", StatusActive))
			if err != nil {
				t.Fatal(err)
			}
			if err := db.UpdateAccountFieldsWithPriority(id, "", "", true, true, nil); err != nil {
				t.Fatal(err)
			}
			stale, err := db.GetAccount(id)
			if err != nil {
				t.Fatal(err)
			}
			if !stale.PaidFallback || stale.PaidCoolingUntil != 0 || stale.ZCodeJWT == "" {
				t.Fatal("fixture must start with eligible dual-channel snapshot")
			}
			// Deterministic stand-in for a DB change while this snapshot waits for a slot.
			switch guard {
			case "cooling":
				err = db.SetAccountPaidStatus(id, "test cooldown", time.Now().Add(time.Hour).Unix())
			case "fallback_disabled":
				err = db.UpdateAccountFieldsWithPriority(id, "", "", true, false, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			z, calls := reviewAdmissionAPI(t, p, db)
			w := httptest.NewRecorder()
			out := z.forwardOnce(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), stale,
				[]byte(`{"model":"GLM-5.3","messages":[{"role":"user","content":"hello"}]}`),
				"", "", true, 1, &relayCtx{provider: "zai", proto: protocolAnthropic}, time.Now(), "apikey", ChannelPaid)
			want := int32(0)
			if guard == "eligible" {
				want = 1
			}
			if got := calls.Load(); got != want {
				t.Fatalf("paid dispatches = %d, want %d after DB %s; response=%s", got, want, guard, w.Body.String())
			}
			if guard != "eligible" && out != outcomeNextAccount {
				t.Fatalf("blocked paid dispatch outcome = %v, want outcomeNextAccount", out)
			}
		})
	}
}

// Done is evaluated on entry to the blocking select, after occupancy was checked.
type reviewAdmissionWaitContext struct {
	context.Context
	waiting chan struct{}
}

func (c *reviewAdmissionWaitContext) Done() <-chan struct{} {
	c.waiting <- struct{}{}
	return c.Context.Done()
}

func TestReviewWaitingSlotReloadsReducedCap(t *testing.T) {
	p, db := newPaidTestPool(t)
	if err := db.SetSetting("max_concurrent_per_account", "2"); err != nil {
		t.Fatal(err)
	}
	a := mkDualAccount("review-waiting-cap", StatusActive)
	releaseFirst, ok := p.AcquireAccountSlot(context.Background(), a, ChannelPaid, 0)
	if !ok {
		t.Fatal("first slot unavailable")
	}
	defer releaseFirst()
	releaseSecond, ok := p.AcquireAccountSlot(context.Background(), a, ChannelPaid, 0)
	if !ok {
		t.Fatal("second slot unavailable")
	}
	defer releaseSecond()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &reviewAdmissionWaitContext{Context: base, waiting: make(chan struct{}, 2)}
	result := make(chan bool, 1)
	go func() {
		release, acquired := p.AcquireAccountSlot(ctx, a, ChannelPaid, 5*time.Second)
		release()
		result <- acquired
	}()
	select {
	case <-ctx.waiting:
	case <-result:
		t.Fatal("waiter returned before a slot was released")
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never entered blocking select")
	}
	if err := db.SetSetting("max_concurrent_per_account", "1"); err != nil {
		t.Fatal(err)
	}
	// No new acquire may refresh the cached cap on the waiter's behalf.
	releaseFirst()
	select {
	case acquired := <-result:
		t.Fatalf("waiter returned acquired=%t while one slot still occupies reduced cap", acquired)
	case <-ctx.waiting:
		// Still full at the new cap; releasing the last holder must admit it.
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not recheck occupancy after release")
	}
	releaseSecond()
	select {
	case acquired := <-result:
		if !acquired {
			t.Fatal("waiter failed after occupancy fell below reduced cap")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not acquire released slot")
	}
}
