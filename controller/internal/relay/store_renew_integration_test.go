package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRecordRenewedCertIntegration verifies the renewal write path (D-19)
// against real Postgres: recording, retry supersede, renewal chains, refused
// states, and the FOR UPDATE renew-vs-revoke serialization.
// Requires PKI_TEST_DATABASE_URL; otherwise skips.
func TestRecordRenewedCertIntegration(t *testing.T) {
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
	notAfter := time.Now().Add(30 * 24 * time.Hour)

	// provisionedRelay creates an active relay holding certificate `serial`.
	provisionedRelay := func(t *testing.T, name, serial string) string {
		t.Helper()
		id, err := store.CreateRelay(ctx, name, []string{}, []string{})
		if err != nil {
			t.Fatalf("create relay: %v", err)
		}
		if err := store.MarkProvisioned(ctx, id, serial, notAfter, "1.0.0", "relay-a"); err != nil {
			t.Fatalf("mark provisioned: %v", err)
		}
		return id
	}

	t.Run("records the renewed cert and updates the relay row", func(t *testing.T) {
		id := provisionedRelay(t, "renew-basic", "p-basic")
		superseded, err := store.RecordRenewedCert(ctx, id, "p-basic", "n-basic", notAfter)
		if err != nil || superseded != 0 {
			t.Fatalf("RecordRenewedCert = %d, %v; want 0, nil", superseded, err)
		}
		assertCertRevoked(t, ctx, store, id, "n-basic", false)
		assertCertRevoked(t, ctx, store, id, "p-basic", false) // old cert expires naturally
		row, err := store.LoadRelayByID(ctx, id)
		if err != nil {
			t.Fatalf("LoadRelayByID: %v", err)
		}
		if row.CertSerial == nil || *row.CertSerial != "n-basic" {
			t.Fatalf("relays.cert_serial = %v, want n-basic", row.CertSerial)
		}
	})

	t.Run("retry from the same presented cert supersedes the earlier successor", func(t *testing.T) {
		id := provisionedRelay(t, "renew-retry", "p-retry")
		if _, err := store.RecordRenewedCert(ctx, id, "p-retry", "n1-retry", notAfter); err != nil {
			t.Fatalf("first renewal: %v", err)
		}
		superseded, err := store.RecordRenewedCert(ctx, id, "p-retry", "n2-retry", notAfter)
		if err != nil || superseded != 1 {
			t.Fatalf("retry renewal = %d, %v; want 1 superseded", superseded, err)
		}
		assertCertRevoked(t, ctx, store, id, "n1-retry", true)
		assertCertRevoked(t, ctx, store, id, "n2-retry", false)
		assertCertRevoked(t, ctx, store, id, "p-retry", false)
		if n := unrevokedCerts(t, ctx, pool, id); n != 2 {
			t.Fatalf("unrevoked certs = %d, want 2 (presented + one successor)", n)
		}
		var reason *string
		if err := pool.QueryRow(ctx, `SELECT revocation_reason FROM relay_certificates WHERE serial = 'n1-retry'`).Scan(&reason); err != nil {
			t.Fatalf("read reason: %v", err)
		}
		if reason == nil || *reason != supersededRenewalReason {
			t.Fatalf("revocation_reason = %v, want %q", reason, supersededRenewalReason)
		}
	})

	t.Run("renewing from the successor does not revoke older certs", func(t *testing.T) {
		id := provisionedRelay(t, "renew-chain", "p-chain")
		if _, err := store.RecordRenewedCert(ctx, id, "p-chain", "n1-chain", notAfter); err != nil {
			t.Fatalf("first renewal: %v", err)
		}
		superseded, err := store.RecordRenewedCert(ctx, id, "n1-chain", "n2-chain", notAfter)
		if err != nil || superseded != 0 {
			t.Fatalf("chained renewal = %d, %v; want 0 superseded", superseded, err)
		}
		for _, serial := range []string{"p-chain", "n1-chain", "n2-chain"} {
			assertCertRevoked(t, ctx, store, id, serial, false)
		}
	})

	t.Run("refuses non-renewable relays and invalid presented certs", func(t *testing.T) {
		id := provisionedRelay(t, "renew-refuse", "p-refuse")

		if _, err := store.RecordRenewedCert(ctx, id, "unknown-serial", "n-x", notAfter); !errors.Is(err, ErrPresentedCertInvalid) {
			t.Fatalf("unknown presented serial: err = %v, want ErrPresentedCertInvalid", err)
		}
		if _, err := store.RevokeRelay(ctx, id, "test", nil); err != nil {
			t.Fatalf("RevokeRelay: %v", err)
		}
		if _, err := store.RecordRenewedCert(ctx, id, "p-refuse", "n-refuse", notAfter); !errors.Is(err, ErrRelayNotRenewable) {
			t.Fatalf("revoked relay: err = %v, want ErrRelayNotRenewable", err)
		}
		if known, _, _ := store.RelayCertStatus(ctx, id, "n-refuse"); known {
			t.Fatal("a cert was recorded for a revoked relay")
		}
	})

	t.Run("pending relay cannot renew", func(t *testing.T) {
		id, err := store.CreateRelay(ctx, "renew-pending", []string{}, []string{})
		if err != nil {
			t.Fatalf("create relay: %v", err)
		}
		if _, err := store.RecordRenewedCert(ctx, id, "p-pending", "n-pending", notAfter); !errors.Is(err, ErrRelayNotRenewable) {
			t.Fatalf("pending relay: err = %v, want ErrRelayNotRenewable", err)
		}
	})

	t.Run("concurrent renew and revoke never leave a live cert", func(t *testing.T) {
		const iterations = 25
		renewFirst, revokeFirst := 0, 0
		for i := 0; i < iterations; i++ {
			presented := fmt.Sprintf("p-race-%d", i)
			renewed := fmt.Sprintf("n-race-%d", i)
			id := provisionedRelay(t, fmt.Sprintf("renew-race-%d", i), presented)

			var wg sync.WaitGroup
			var renewErr, revokeErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, renewErr = store.RecordRenewedCert(ctx, id, presented, renewed, notAfter)
			}()
			go func() {
				defer wg.Done()
				_, revokeErr = store.RevokeRelay(ctx, id, "race", nil)
			}()
			wg.Wait()

			if revokeErr != nil {
				t.Fatalf("iteration %d: RevokeRelay: %v", i, revokeErr)
			}
			if renewErr != nil && !errors.Is(renewErr, ErrRelayNotRenewable) {
				t.Fatalf("iteration %d: RecordRenewedCert: %v", i, renewErr)
			}
			if n := unrevokedCerts(t, ctx, pool, id); n != 0 {
				t.Fatalf("iteration %d: %d unrevoked cert(s) remain for a revoked relay (renewErr=%v)", i, n, renewErr)
			}
			if renewErr == nil {
				// The renewal won the lock: the later revoke must have revoked it,
				// so it is published on the relay CRL.
				renewFirst++
				assertOnRevokedList(t, ctx, store, renewed)
			} else {
				revokeFirst++
			}
		}
		t.Logf("orderings observed: renew-first=%d revoke-first=%d", renewFirst, revokeFirst)
	})
}

func assertCertRevoked(t *testing.T, ctx context.Context, store *Store, relayID, serial string, wantRevoked bool) {
	t.Helper()
	known, revoked, err := store.RelayCertStatus(ctx, relayID, serial)
	if err != nil {
		t.Fatalf("RelayCertStatus %s: %v", serial, err)
	}
	if !known {
		t.Fatalf("cert %s not recorded", serial)
	}
	if revoked != wantRevoked {
		t.Fatalf("cert %s revoked = %v, want %v", serial, revoked, wantRevoked)
	}
}

func unrevokedCerts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, relayID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM relay_certificates WHERE relay_id = $1 AND revoked_at IS NULL`, relayID,
	).Scan(&n); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("count unrevoked certs: %v", err)
	}
	return n
}

func assertOnRevokedList(t *testing.T, ctx context.Context, store *Store, serial string) {
	t.Helper()
	serials, err := store.ListRevokedRelaySerials(ctx)
	if err != nil {
		t.Fatalf("ListRevokedRelaySerials: %v", err)
	}
	for _, s := range serials {
		if s.Serial == serial {
			return
		}
	}
	t.Fatalf("renewed serial %s missing from the revoked list", serial)
}
