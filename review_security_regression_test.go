package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestReviewProxyPasswordHealthAndSelection(t *testing.T) {
	for _, kind := range []string{"encrypted", "wrong-key", "legacy-plaintext", "empty"} {
		t.Run(kind, func(t *testing.T) {
			db, _ := newOpsTestDB(t)
			password := "proxy-password"
			if kind == "empty" {
				password = ""
			}
			id, err := db.SaveProxyNode(&ProxyNode{
				Name: "review", Type: "http", Host: "127.0.0.1", Port: 8080,
				Username: "proxy-user", Password: password, GroupName: "review-group",
				IsDefault: true, Enabled: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := db.conn.QueryRow(`SELECT password FROM proxy_nodes WHERE id = ?`, id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if password != "" && !strings.HasPrefix(raw, vaultPrefix) {
				t.Fatal("fixture password not encrypted at rest")
			}
			switch kind {
			case "wrong-key":
				setVaultSeedOverride("review-different-vault-key")
			case "legacy-plaintext":
				if _, err := db.conn.Exec(`UPDATE proxy_nodes SET password = ? WHERE id = ?`, password, id); err != nil {
					t.Fatal(err)
				}
			}
			nodes, err := db.ListProxyNodes()
			if err != nil || len(nodes) != 1 {
				t.Fatalf("ListProxyNodes: count=%d err=%v", len(nodes), err)
			}
			broken := kind == "wrong-key"
			if broken {
				password = ""
			}
			if nodes[0].Password != password || nodes[0].PasswordBroken != broken {
				t.Errorf("password=%q broken=%v; want password=%q broken=%v", nodes[0].Password, nodes[0].PasswordBroken, password, broken)
			}
			for _, group := range []string{"review-group", "unbound-group", ""} {
				got, err := db.ProxyNodeForGroup(group)
				if err != nil {
					t.Fatal(err)
				}
				if broken {
					if got != nil {
						t.Errorf("group %q selected broken authenticated node", group)
					}
				} else if got == nil || got.ID != id {
					t.Errorf("group %q did not select healthy node %d", group, id)
				}
			}
		})
	}
}

func TestReviewSwitchBackJWTOnlyClearsPriorIdentity(t *testing.T) {
	home := restoreTestEnv(t)
	db, _ := newOpsTestDB(t)
	m := &AccountManager{db: db}
	f := localClientFilesFor(home)
	if err := os.MkdirAll(filepath.Dir(f.credentials), 0700); err != nil {
		t.Fatal(err)
	}
	unrelatedCreds := map[string]string{
		"oauth:other:access_token": "other-access", "oauth:other:user_info": "other-info", "bot-token": "bot-secret",
	}
	if err := atomicWriteJSON(f.credentials, unrelatedCreds); err != nil {
		t.Fatal(err)
	}
	unrelatedProvider := map[string]interface{}{
		"enabled": true, "options": map[string]interface{}{"apiKey": "other-paid-key"},
	}
	if err := atomicWriteJSON(f.config, map[string]interface{}{
		"provider": map[string]interface{}{"unrelated-provider": unrelatedProvider}, "theme": "dark",
	}); err != nil {
		t.Fatal(err)
	}
	accounts := []*Account{
		{UserID: "review-A", Provider: "zai", AuthType: "jwt", ZCodeJWT: "jwt-A", AccessToken: "access-A", UserInfo: `{"id":"A"}`, APIKey: "paid-A", Status: StatusActive, Enabled: true},
		{UserID: "review-B", Provider: "zai", AuthType: "jwt", ZCodeJWT: "jwt-B", Status: StatusActive, Enabled: true},
	}
	for _, a := range accounts {
		id, err := db.UpsertAccount(a)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.SwitchBackToLocal(id, false); err != nil {
			t.Fatal(err)
		}
	}
	var creds map[string]string
	data, err := os.ReadFile(f.credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"oauth:zai:access_token", "oauth:zai:user_info"} {
		if creds[key] != "" {
			t.Errorf("JWT-only account retained previous identity field %s", key)
		}
	}
	for key, want := range unrelatedCreds {
		if creds[key] != want {
			t.Errorf("unrelated credential %s changed", key)
		}
	}
	for key, want := range map[string]string{"zcodejwttoken": "jwt-B", "oauth:active_provider": "zai"} {
		got, err := DecryptCredential(creds[key], DefaultCredentialSecret(home))
		if err != nil || got != want {
			t.Errorf("%s=%q err=%v; want %q", key, got, err, want)
		}
	}
	var cfg map[string]interface{}
	data, err = os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	providers, ok := cfg["provider"].(map[string]interface{})
	if !ok {
		t.Fatal("provider config missing")
	}
	if !reflect.DeepEqual(providers["unrelated-provider"], unrelatedProvider) || cfg["theme"] != "dark" {
		t.Error("unrelated provider or config changed")
	}
	if paid, ok := providers["builtin:zai-coding-plan"].(map[string]interface{}); ok {
		opts, _ := paid["options"].(map[string]interface{})
		if paid["enabled"] != false && opts["apiKey"] != nil && opts["apiKey"] != "" {
			t.Error("previous paid API key remains usable: neither disabled nor removed")
		}
	}
	start, _ := providers["builtin:zai-start-plan"].(map[string]interface{})
	opts, _ := start["options"].(map[string]interface{})
	if start["enabled"] != true || opts["apiKey"] != "jwt-B" {
		t.Error("JWT-only account start plan not enabled with new JWT")
	}
}

func TestReviewPlanAPIRejectsScheduledReset(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		for _, task := range []string{"reset", "claim", "detect", "activate", ""} {
			t.Run(method+"/"+task, func(t *testing.T) {
				db, _ := newOpsTestDB(t)
				s := &APIServer{db: db}
				var id int64
				if method == http.MethodPut {
					var err error
					id, err = db.SaveClaimPlan(&ClaimPlan{PlanName: "original", CronExpr: "0 0 * * *", TaskType: "claim", TargetType: "all_accounts"})
					if err != nil {
						t.Fatal(err)
					}
				}
				r := httptest.NewRequest(method, "/api/plans", strings.NewReader(`{"plan_name":"replacement","cron_expr":"0 0 * * *","is_active":true,"task_type":"`+task+`"}`))
				if id != 0 {
					r.SetPathValue("id", strconv.FormatInt(id, 10))
				}
				w := httptest.NewRecorder()
				s.handleSavePlan(w, r)
				wantStatus := http.StatusOK
				if task == "reset" {
					wantStatus = http.StatusBadRequest
				}
				if w.Code != wantStatus {
					t.Errorf("status=%d body=%s; want %d", w.Code, w.Body.String(), wantStatus)
				}
				var resets int
				if err := db.conn.QueryRow(`SELECT count(*) FROM claim_plans WHERE task_type = 'reset'`).Scan(&resets); err != nil {
					t.Fatal(err)
				}
				if resets != 0 {
					t.Error("manual-only reset persisted as scheduled plan")
				}
				if task == "reset" && id != 0 {
					p, err := db.GetClaimPlan(id)
					if err != nil || p.TaskType != "claim" || p.PlanName != "original" {
						t.Errorf("rejected update changed existing plan: %+v err=%v", p, err)
					}
				}
			})
		}
	}
}

// Sequential guard only: existing locks offer no deterministic signal between
// Login's password verification and session insertion. Pending-login race (F7)
// remains uncovered rather than relying on sleeps or scheduler ordering.
func TestReviewPasswordRotationInvalidatesSessions(t *testing.T) {
	db, _ := newOpsTestDB(t)
	hash, err := hashPassword("review-old-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetPasswordHash(hash); err != nil {
		t.Fatal(err)
	}
	am := &AuthManager{db: db, sessions: make(map[string]*sessionEntry)}
	token, ok, _ := am.Login("admin", "review-old-password", "127.0.0.1")
	if !ok || !am.IsValid(token) {
		t.Fatal("initial login failed")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/change-password", strings.NewReader(`{"old_password":"review-old-password","new_password":"review-new-password"}`))
	am.HandleChangePassword(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("rotation status=%d body=%s", w.Code, w.Body.String())
	}
	if am.IsValid(token) {
		t.Error("pre-rotation session remains valid")
	}
	if _, ok, _ := am.Login("admin", "review-old-password", "127.0.0.1"); ok {
		t.Error("old password accepted after rotation")
	}
	if token, ok, _ := am.Login("admin", "review-new-password", "127.0.0.1"); !ok || !am.IsValid(token) {
		t.Error("new password cannot create valid session")
	}
}
