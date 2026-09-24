package relay

import (
	"context"
	"net"
	"testing"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"google.golang.org/grpc/codes"
)

// Sprint 20 Phase C — relay SAN allowlist enforcement.
//
// Invariants under test:
//  1. The provisioning token is consumed only after ALL validation succeeds.
//  2. The signer receives the STORED allowlists, never the relay-supplied SANs.
//  3. Both the request SANs and the CSR SANs are checked against the stored
//     allowlists (DNS normalized; IPs compared as parsed net.IP values).

type provisionRun struct {
	ctx   context.Context
	fake  *fakePKI
	store *fakeProvisionStore
	run   func(req *relaypb.ProvisionRequest) error
	// tokenUnused reports whether the provisioning JTI is still in Valkey.
	tokenUnused func() bool
}

func newProvisionRun(t *testing.T, store *fakeProvisionStore) *provisionRun {
	t.Helper()
	ctx := context.Background()
	rdb := newProvisionTestValkey(t)
	token, jti := storeProvisioningToken(t, ctx, rdb, testRelayID)
	fake := newFakePKI()
	service := newProvisionTestService(fake, store, rdb)
	return &provisionRun{
		ctx:   ctx,
		fake:  fake,
		store: store,
		run: func(req *relaypb.ProvisionRequest) error {
			req.ProvisioningToken = token
			_, err := service.Provision(ctx, req)
			return err
		},
		tokenUnused: func() bool { return jtiPresent(t, ctx, rdb, jti) },
	}
}

func provisionRequest(t *testing.T, dnsSANs, ipSANs, csrDNS, csrIPs []string) *relaypb.ProvisionRequest {
	t.Helper()
	return &relaypb.ProvisionRequest{
		RelayId:  testRelayID,
		CsrDer:   makeRelayCSRDER(t, testRelayID, csrDNS, csrIPs),
		DnsSans:  dnsSANs,
		IpSans:   ipSANs,
		Version:  "1.0.0",
		Hostname: "relay-test",
	}
}

// assertRejectedTokenIntact: rejected with want, nothing signed, row not
// marked, and the provisioning token still usable.
func assertRejectedTokenIntact(t *testing.T, r *provisionRun, err error, want codes.Code) {
	t.Helper()
	assertStatusCode(t, err, want)
	if r.fake.signCalls != 0 {
		t.Fatalf("SignRelayCert calls = %d, want 0", r.fake.signCalls)
	}
	if r.store.markedRelayID != "" {
		t.Fatalf("relay %q marked provisioned by a rejected request", r.store.markedRelayID)
	}
	if !r.tokenUnused() {
		t.Fatal("provisioning token was consumed by a rejected request")
	}
}

// TestProvisionSignerReceivesStoredAllowlists — invariant 2: the relay asks for
// a subset of its allowlist; the signer is handed the full STORED allowlist,
// not the relay-supplied SANs. The token is consumed on success.
func TestProvisionSignerReceivesStoredAllowlists(t *testing.T) {
	store := pendingRelayStore(
		[]string{"relay.example.com", "alt.example.com"},
		[]string{"203.0.113.10", "198.51.100.7"},
	)
	r := newProvisionRun(t, store)
	req := provisionRequest(t,
		[]string{"relay.example.com"}, []string{"203.0.113.10"},
		[]string{"relay.example.com"}, []string{"203.0.113.10"})

	if err := r.run(req); err != nil {
		t.Fatalf("Provision rejected an in-allowlist request: %v", err)
	}
	if len(r.fake.dnsNames) != 2 || r.fake.dnsNames[0] != "relay.example.com" || r.fake.dnsNames[1] != "alt.example.com" {
		t.Fatalf("signer DNS allowlist = %v, want the stored list", r.fake.dnsNames)
	}
	if len(r.fake.ipAddrs) != 2 || !r.fake.ipAddrs[0].Equal(net.ParseIP("203.0.113.10")) || !r.fake.ipAddrs[1].Equal(net.ParseIP("198.51.100.7")) {
		t.Fatalf("signer IP allowlist = %v, want the stored list", r.fake.ipAddrs)
	}
	if r.tokenUnused() {
		t.Fatal("successful provisioning did not consume the token")
	}
}

// TestProvisionRejectsRequestSANOutsideAllowlist — invariant 3 (request SANs).
func TestProvisionRejectsRequestSANOutsideAllowlist(t *testing.T) {
	r := newProvisionRun(t, defaultRelayStore())
	req := provisionRequest(t,
		[]string{"relay.example.com", "login.bank.example"}, []string{"203.0.113.10"},
		[]string{"relay.example.com"}, []string{"203.0.113.10"})

	assertRejectedTokenIntact(t, r, r.run(req), codes.PermissionDenied)
}

// TestProvisionRejectsCSRSANOutsideAllowlist — invariant 3 (CSR SANs): the
// request declares nothing, but the CSR smuggles a DNS SAN. Rejected by the
// pre-burn check, not by the signer after the burn.
func TestProvisionRejectsCSRSANOutsideAllowlist(t *testing.T) {
	r := newProvisionRun(t, defaultRelayStore())
	req := provisionRequest(t, nil, nil, []string{"login.bank.example"}, nil)

	assertRejectedTokenIntact(t, r, r.run(req), codes.PermissionDenied)
}

// TestProvisionRejectsCSRIPOutsideAllowlist — same for a smuggled CSR IP SAN.
func TestProvisionRejectsCSRIPOutsideAllowlist(t *testing.T) {
	r := newProvisionRun(t, defaultRelayStore())
	req := provisionRequest(t, nil, nil, nil, []string{"10.0.0.1"})

	assertRejectedTokenIntact(t, r, r.run(req), codes.PermissionDenied)
}

// TestProvisionEmptyAllowlistAcceptsURIOnlyCSR — empty stored lists forbid
// DNS/IP SANs but a SPIFFE-URI-only CSR still provisions.
func TestProvisionEmptyAllowlistAcceptsURIOnlyCSR(t *testing.T) {
	r := newProvisionRun(t, pendingRelayStore(nil, nil))
	req := provisionRequest(t, nil, nil, nil, nil)

	if err := r.run(req); err != nil {
		t.Fatalf("Provision rejected a URI-only CSR with empty allowlists: %v", err)
	}
	if len(r.fake.dnsNames) != 0 || len(r.fake.ipAddrs) != 0 {
		t.Fatalf("signer allowlists = dns %v ip %v, want both empty", r.fake.dnsNames, r.fake.ipAddrs)
	}
}

// TestProvisionEmptyAllowlistRejectsRequestedIP — empty ip_allowlist forbids
// IP SANs (the local-dev RELAY_IP_SANS=127.0.0.1 case).
func TestProvisionEmptyAllowlistRejectsRequestedIP(t *testing.T) {
	r := newProvisionRun(t, pendingRelayStore(nil, nil))
	req := provisionRequest(t, nil, []string{"127.0.0.1"}, nil, []string{"127.0.0.1"})

	assertRejectedTokenIntact(t, r, r.run(req), codes.PermissionDenied)
}

// TestProvisionRejectsRelayNotPending — an already-provisioned, inactive,
// revoked or deleted relay cannot be (re)provisioned; the token survives.
func TestProvisionRejectsRelayNotPending(t *testing.T) {
	for _, st := range []string{"active", "inactive", "revoked", "deleted"} {
		t.Run(st, func(t *testing.T) {
			store := defaultRelayStore()
			store.row.Status = st
			r := newProvisionRun(t, store)

			assertRejectedTokenIntact(t, r, r.run(validProvisionRequest(t)), codes.FailedPrecondition)
		})
	}
}

// TestProvisionRejectsCSRIdentityBeforeBurn — invariant 1 covers every signer
// check, not just SANs: a CSR for another relay's SPIFFE ID is refused while
// the token is still unused.
func TestProvisionRejectsCSRIdentityBeforeBurn(t *testing.T) {
	r := newProvisionRun(t, defaultRelayStore())
	req := validProvisionRequest(t)
	req.CsrDer = makeRelayCSRDER(t, otherTestRelayID, []string{"relay.example.com"}, []string{"203.0.113.10"})

	assertRejectedTokenIntact(t, r, r.run(req), codes.InvalidArgument)
}

// TestProvisionRetryAfterRejectionSucceedsWithSameToken — AT-C.5: a rejected
// request leaves the token usable, so the corrected request succeeds with it.
func TestProvisionRetryAfterRejectionSucceedsWithSameToken(t *testing.T) {
	r := newProvisionRun(t, defaultRelayStore())
	bad := provisionRequest(t,
		[]string{"relay.example.com"}, []string{"203.0.113.10", "10.0.0.5"},
		[]string{"relay.example.com"}, []string{"203.0.113.10", "10.0.0.5"})
	assertRejectedTokenIntact(t, r, r.run(bad), codes.PermissionDenied)

	if err := r.run(validProvisionRequest(t)); err != nil {
		t.Fatalf("corrected request with the same token was rejected: %v", err)
	}
	if r.fake.signCalls != 1 || r.store.markedRelayID != testRelayID {
		t.Fatalf("signCalls=%d marked=%q, want 1 / %q", r.fake.signCalls, r.store.markedRelayID, testRelayID)
	}
}

// --- canonicalization -------------------------------------------------------

// TestProvisionNormalizesRequestDNS — request DNS SANs are compared
// case-insensitively and with a trailing root dot stripped.
func TestProvisionNormalizesRequestDNS(t *testing.T) {
	r := newProvisionRun(t, defaultRelayStore())
	req := provisionRequest(t,
		[]string{"Relay.Example.COM."}, []string{"203.0.113.10"},
		[]string{"relay.example.com"}, []string{"203.0.113.10"})

	if err := r.run(req); err != nil {
		t.Fatalf("mixed-case/trailing-dot request DNS SAN rejected: %v", err)
	}
}

// TestProvisionRejectsNonCanonicalCSRDNSBeforeBurn — the signer copies CSR DNS
// names verbatim and matches them exactly, so a mixed-case CSR name must be
// refused BEFORE the burn (a normalized-only check would pass it here and the
// signer would then fail after consuming the token).
func TestProvisionRejectsNonCanonicalCSRDNSBeforeBurn(t *testing.T) {
	for _, name := range []string{"Relay.Example.com", "relay.example.com."} {
		t.Run(name, func(t *testing.T) {
			r := newProvisionRun(t, defaultRelayStore())
			req := provisionRequest(t, []string{"relay.example.com"}, nil, []string{name}, nil)

			assertRejectedTokenIntact(t, r, r.run(req), codes.InvalidArgument)
		})
	}
}

// TestProvisionComparesIPv6AsParsedValues — a non-canonical IPv6 text form in
// the request matches the stored canonical form; a different address does not.
func TestProvisionComparesIPv6AsParsedValues(t *testing.T) {
	stored := pendingRelayStore(nil, []string{"2001:db8::1"})

	t.Run("equivalent textual form accepted", func(t *testing.T) {
		store := *stored
		r := newProvisionRun(t, &store)
		req := provisionRequest(t, nil, []string{"2001:DB8:0:0::1"}, nil, []string{"2001:db8::1"})

		if err := r.run(req); err != nil {
			t.Fatalf("equivalent IPv6 SAN rejected: %v", err)
		}
		if len(r.fake.ipAddrs) != 1 || !r.fake.ipAddrs[0].Equal(net.ParseIP("2001:db8::1")) {
			t.Fatalf("signer IP allowlist = %v, want [2001:db8::1]", r.fake.ipAddrs)
		}
	})

	t.Run("different address rejected", func(t *testing.T) {
		store := *stored
		r := newProvisionRun(t, &store)
		req := provisionRequest(t, nil, []string{"2001:db8::2"}, nil, []string{"2001:db8::2"})

		assertRejectedTokenIntact(t, r, r.run(req), codes.PermissionDenied)
	})
}

func TestCanonicalDNSName(t *testing.T) {
	for in, want := range map[string]string{
		"relay.example.com":  "relay.example.com",
		"Relay.Example.COM":  "relay.example.com",
		"relay.example.com.": "relay.example.com",
		"RELAY.EXAMPLE.COM.": "relay.example.com",
		"localhost":          "localhost",
	} {
		got, err := canonicalDNSName(in)
		if err != nil || got != want {
			t.Fatalf("canonicalDNSName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, invalid := range []string{"", ".", "relay.example.com..", "*.example.com", "relay example.com", "relay/example"} {
		if got, err := canonicalDNSName(invalid); err == nil {
			t.Fatalf("canonicalDNSName(%q) = %q, want error", invalid, got)
		}
	}
}

func TestRequestSANParsingDedupesCanonicalForms(t *testing.T) {
	if _, err := canonicalRequestDNSSANs([]string{"relay.example.com", "RELAY.example.com."}); err == nil {
		t.Fatal("DNS SANs equal after normalization were not reported as duplicates")
	}
	if _, err := parseRequestIPSANs([]string{"2001:db8::1", "2001:DB8:0:0::1"}); err == nil {
		t.Fatal("IPv6 SANs equal as parsed values were not reported as duplicates")
	}
	if _, err := parseRequestIPSANs([]string{"not-an-ip"}); err == nil {
		t.Fatal("invalid IP SAN accepted")
	}
}
