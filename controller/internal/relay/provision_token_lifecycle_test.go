package relay

import (
	"context"
	"crypto/x509"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/pki"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Provisioning-token lifecycle (Sprint 22 Phase E, security audit E-2): an
// expired token is refused without being consumed, and one token provisions a
// relay exactly once even when used concurrently.

func TestProvisionRejectsExpiredToken(t *testing.T) {
	ctx := context.Background()
	rdb := newProvisionTestValkey(t)

	// A correctly signed token whose exp is an hour in the past, with its jti
	// still live in Valkey: only the JWT expiry can refuse it.
	expired, expiredJTI, err := IssueProvisioningToken(testProvisioningSecret, testRelayID, -time.Hour)
	if err != nil {
		t.Fatalf("issue expired token: %v", err)
	}
	if err := StoreProvisioningJTI(ctx, rdb, expiredJTI, testRelayID, time.Hour); err != nil {
		t.Fatalf("store expired token jti: %v", err)
	}

	fake := newFakePKI()
	service := newProvisionTestService(fake, defaultRelayStore(), rdb)
	req := validProvisionRequest(t)
	req.ProvisioningToken = expired
	_, err = service.Provision(ctx, req)
	assertStatusCode(t, err, codes.PermissionDenied)
	if fake.signCalls != 0 {
		t.Fatalf("SignRelayCert ran %d time(s) for an expired token", fake.signCalls)
	}
	if !jtiPresent(t, ctx, rdb, expiredJTI) {
		t.Fatal("expired token's jti was consumed; it must be refused before the burn")
	}

	// Positive control: the same relay, request shape and Valkey state with an
	// unexpired token provisions, so the refusal above was the expiry.
	valid, validJTI := storeProvisioningToken(t, ctx, rdb, testRelayID)
	req = validProvisionRequest(t)
	req.ProvisioningToken = valid
	if _, err := service.Provision(ctx, req); err != nil {
		t.Fatalf("positive control: valid token rejected: %v", err)
	}
	if fake.signCalls != 1 {
		t.Fatalf("positive control: SignRelayCert calls = %d, want 1", fake.signCalls)
	}
	if jtiPresent(t, ctx, rdb, validJTI) {
		t.Fatal("positive control: valid token's jti was not consumed")
	}
}

// countingPKI is a concurrency-safe signer: a broken implementation that signs
// more than once fails the count assertion instead of racing on a field.
type countingPKI struct {
	*fakePKI
	calls atomic.Int32
}

func (c *countingPKI) SignRelayCert(context.Context, string, *x509.CertificateRequest, []string, []net.IP, time.Duration) (*pki.RelayCertResult, error) {
	c.calls.Add(1)
	return c.fakePKI.result, nil
}

func TestProvisionTokenSingleUseUnderConcurrency(t *testing.T) {
	t.Run("miniredis", func(t *testing.T) {
		runConcurrentProvision(t, newProvisionTestValkey(t))
	})
	// The same contention against a real Valkey server, whose GETDEL is the
	// production burn. Runs when the isolated test Valkey is configured.
	t.Run("valkey", func(t *testing.T) {
		raw := os.Getenv("AUTH_TEST_VALKEY_URL")
		if raw == "" {
			t.Skip("AUTH_TEST_VALKEY_URL not set")
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			t.Fatalf("bad AUTH_TEST_VALKEY_URL %q: %v", raw, err)
		}
		client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{u.Host}, DisableCache: true})
		if err != nil {
			t.Fatalf("valkey client: %v", err)
		}
		t.Cleanup(client.Close)
		runConcurrentProvision(t, valkeycompat.NewAdapter(client))
	})
}

func runConcurrentProvision(t *testing.T, rdb valkeycompat.Cmdable) {
	t.Helper()
	ctx := context.Background()
	token, jti := storeProvisioningToken(t, ctx, rdb, testRelayID)
	t.Cleanup(func() { _ = rdb.Del(context.Background(), provisioningJTIPrefix+jti).Err() })

	signer := &countingPKI{fakePKI: newFakePKI()}
	service := NewService(signer, defaultRelayStore(), time.Hour).
		WithHeartbeatCache(rdb, time.Minute).
		WithProvisioningAuth(testProvisioningSecret)

	const n = 16
	// Requests (each with its own CSR) are built before the barrier so the
	// calls themselves start together and contend for the token.
	reqs := make([]*relaypb.ProvisionRequest, n)
	for i := range reqs {
		reqs[i] = validProvisionRequest(t)
		reqs[i].ProvisioningToken = token
	}
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			_, errs[i] = service.Provision(ctx, reqs[i])
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	successes := 0
	for i, err := range errs {
		if err == nil {
			successes++
			continue
		}
		if status.Code(err) != codes.PermissionDenied || !strings.Contains(status.Convert(err).Message(), "already used") {
			t.Errorf("call %d: %v, want PermissionDenied \"provisioning token already used or unknown\"", i, err)
		}
	}
	if successes != 1 {
		t.Errorf("%d of %d concurrent calls succeeded with one token, want exactly 1", successes, n)
	}
	if got := signer.calls.Load(); got != 1 {
		t.Errorf("SignRelayCert ran %d time(s), want exactly 1", got)
	}
	if jtiPresent(t, ctx, rdb, jti) {
		t.Error("token jti still present after a successful provision")
	}
}
