package relay

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRevokeRelayIntegration verifies Store.RevokeRelay's atomic behavior end to
// end against a real Postgres: revoking marks every unexpired certificate serial
// AND the relay status in one transaction, the in-tx hook error rolls back the
// whole revoke (nothing partially committed), and revoking an unknown relay
// returns ErrRelayNotFound. Requires PKI_TEST_DATABASE_URL; otherwise skips.
func TestRevokeRelayIntegration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueRelayTestDBName(t)

	adminPool := mustConnectRelayPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := withRelayTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectRelayPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyRelayMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	store := NewStore(pool)

	t.Run("revoke marks every unexpired cert and the relay status atomically", func(t *testing.T) {
		relayID, err := store.CreateRelay(ctx, "revoke-test-relay", []string{}, []string{})
		if err != nil {
			t.Fatalf("create relay: %v", err)
		}
		if err := store.RecordIssuedCert(ctx, relayID, "serial-a", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("record cert a: %v", err)
		}
		if err := store.RecordIssuedCert(ctx, relayID, "serial-b", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("record cert b: %v", err)
		}

		var hookCalledWith int
		revoked, err := store.RevokeRelay(ctx, relayID, "compromised", func(ctx context.Context, tx pgx.Tx, n int) error {
			hookCalledWith = n
			return nil
		})
		if err != nil {
			t.Fatalf("RevokeRelay: %v", err)
		}
		if revoked != 2 {
			t.Fatalf("revoked = %d, want 2", revoked)
		}
		if hookCalledWith != 2 {
			t.Fatalf("in-tx hook saw revoked=%d, want 2", hookCalledWith)
		}

		row, err := store.LoadRelayByID(ctx, relayID)
		if err != nil {
			t.Fatalf("LoadRelayByID: %v", err)
		}
		if row.Status != "revoked" {
			t.Fatalf("relay status = %q, want revoked", row.Status)
		}

		serials, err := store.ListRevokedRelaySerials(ctx)
		if err != nil {
			t.Fatalf("ListRevokedRelaySerials: %v", err)
		}
		found := map[string]bool{}
		for _, s := range serials {
			found[s.Serial] = true
		}
		if !found["serial-a"] || !found["serial-b"] {
			t.Fatalf("expected both serials on the revoked list, got %v", serials)
		}

		// Idempotent: a second revoke touches zero already-revoked certs.
		revokedAgain, err := store.RevokeRelay(ctx, relayID, "compromised again", func(ctx context.Context, tx pgx.Tx, n int) error {
			return nil
		})
		if err != nil {
			t.Fatalf("second RevokeRelay: %v", err)
		}
		if revokedAgain != 0 {
			t.Fatalf("second revoke revoked = %d, want 0", revokedAgain)
		}
	})

	t.Run("an in-tx hook error rolls back the entire revoke", func(t *testing.T) {
		relayID, err := store.CreateRelay(ctx, "revoke-rollback-relay", []string{}, []string{})
		if err != nil {
			t.Fatalf("create relay: %v", err)
		}
		if err := store.RecordIssuedCert(ctx, relayID, "serial-rollback", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("record cert: %v", err)
		}

		hookErr := errors.New("audit write failed")
		_, err = store.RevokeRelay(ctx, relayID, "should not stick", func(ctx context.Context, tx pgx.Tx, n int) error {
			return hookErr
		})
		if !errors.Is(err, hookErr) {
			t.Fatalf("RevokeRelay error = %v, want %v", err, hookErr)
		}

		// Nothing must have been committed: status and cert revocation both
		// stay untouched despite the UPDATEs having run inside the transaction.
		row, err := store.LoadRelayByID(ctx, relayID)
		if err != nil {
			t.Fatalf("LoadRelayByID: %v", err)
		}
		if row.Status == "revoked" {
			t.Fatal("relay status was committed despite the in-tx hook failing")
		}

		serials, err := store.ListRevokedRelaySerials(ctx)
		if err != nil {
			t.Fatalf("ListRevokedRelaySerials: %v", err)
		}
		for _, s := range serials {
			if s.Serial == "serial-rollback" {
				t.Fatal("cert revocation was committed despite the in-tx hook failing")
			}
		}
	})

	t.Run("revoking an unknown relay returns ErrRelayNotFound", func(t *testing.T) {
		_, err := store.RevokeRelay(ctx, "00000000-0000-0000-0000-000000000000", "n/a", nil)
		if !errors.Is(err, ErrRelayNotFound) {
			t.Fatalf("RevokeRelay(unknown) error = %v, want %v", err, ErrRelayNotFound)
		}
	})
}

func mustConnectRelayPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping pool: %v", err)
	}
	return pool
}

func applyRelayMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("execute %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func withRelayTestDBName(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func uniqueRelayTestDBName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("relay_revoke_test_%d_%d", os.Getpid(), time.Now().UnixNano())
}
