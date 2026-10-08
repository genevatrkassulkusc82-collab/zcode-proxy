package main

import (
	"strings"
	"testing"
)

func TestReviewImportBundleReportsWriteFailures(t *testing.T) {
	for _, mode := range []string{"success", "database-partial", "vault-degraded", "wrong-password"} {
		t.Run(mode, func(t *testing.T) {
			db, _ := newOpsTestDB(t)
			m := &AccountManager{db: db}
			var ids []int64
			for _, user := range []string{"review-first", "review-second"} {
				id, err := db.UpsertAccount(&Account{
					UserID: user, Email: user + "@example.invalid", Provider: "zai", AuthType: "jwt",
					ZCodeJWT: "jwt-" + user, Status: StatusActive, Enabled: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			// Export orders by created_at; avoid equal timestamps deciding order.
			if _, err := db.conn.Exec(`UPDATE accounts SET created_at = CASE user_id
				WHEN 'review-first' THEN '2020-01-01 00:00:00'
				ELSE '2020-01-02 00:00:00' END`); err != nil {
				t.Fatal(err)
			}
			bundle, err := m.ExportBundle("review-bundle-password", ids)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.conn.Exec(`DELETE FROM accounts`); err != nil {
				t.Fatal(err)
			}
			password := "review-bundle-password"
			wantCount := 2
			switch mode {
			case "database-partial":
				// Abort exactly the second write, not the transaction containing
				// the first account. Import must report failure AND partial progress.
				if _, err := db.conn.Exec(`CREATE TRIGGER review_reject_second BEFORE INSERT ON accounts
					WHEN NEW.user_id = 'review-second'
					BEGIN SELECT RAISE(ABORT, 'review account write denied'); END`); err != nil {
					t.Fatal(err)
				}
				wantCount = 1
			case "vault-degraded":
				resetVaultSeed()
				if !vaultSeedIsDerived() {
					t.Fatal("fixture did not enter degraded vault state")
				}
				wantCount = 0
			case "wrong-password":
				password = "wrong-bundle-password"
				wantCount = 0
			}
			count, err := m.ImportBundle(password, bundle)
			if mode == "success" {
				if err != nil {
					t.Errorf("valid import failed: %v", err)
				}
			} else if err == nil {
				t.Errorf("ImportBundle returned nil error for %s", mode)
			}
			if count != wantCount {
				t.Errorf("import count=%d; want %d", count, wantCount)
			}
			var stored int
			if err := db.conn.QueryRow(`SELECT count(*) FROM accounts`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != wantCount {
				t.Errorf("persisted accounts=%d; want %d", stored, wantCount)
			}
			if wantCount > 0 {
				var jwt string
				if err := db.conn.QueryRow(`SELECT zcode_jwt FROM accounts WHERE user_id = 'review-first'`).Scan(&jwt); err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(jwt, vaultPrefix) || vaultDecrypt(jwt) != "jwt-review-first" {
					t.Error("successful account credentials not preserved and encrypted")
				}
			}
		})
	}
}
