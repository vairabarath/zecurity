package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/metadata"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/shield"
	"github.com/yourorg/ztna/controller/internal/spiffe"
)

// Certificate audit bug 3: revoking a shield only flipped shields.status.
// Nothing recorded the revocation durably, nothing invalidated the policy or
// told connectors, and deleting the row (directly or by cascade) erased it.
// These tests pin the fix: a tombstone and the propagation work commit in the
// same transaction as the revocation; a dispatcher drains that work until it
// has invalidated the policy and pushed the workspace's complete revocation
// list to every connected connector; and a connector receives that list when
// its Control stream opens.

// txPool adapts a pool to shield.TxBeginner.
type txPool struct{ *pgxpool.Pool }

func (p txPool) BeginTx(ctx context.Context) (pgx.Tx, error) { return p.Begin(ctx) }

type shieldRevocationFixture struct {
	pool   *pgxpool.Pool
	ctx    context.Context
	wsID   string
	td     string
	rnID   string
	connID string
}

func newShieldRevocationFixture(t *testing.T) *shieldRevocationFixture {
	t.Helper()
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("shield-rev-%d", time.Now().UnixNano())
	wsID, td, rnID := seedRevocationWorkspace(t, pool, ctx, slug)
	connID := seedConnector(t, pool, ctx, wsID, rnID, td, "active", nil)
	return &shieldRevocationFixture{pool: pool, ctx: ctx, wsID: wsID, td: td, rnID: rnID, connID: connID}
}

func (f *shieldRevocationFixture) seedShield(t *testing.T, status string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, trust_domain)
		 VALUES ($1, $2, $3, 'shield', $4, $5) RETURNING id::text`,
		f.wsID, f.rnID, f.connID, status, f.td,
	).Scan(&id); err != nil {
		t.Fatalf("seed shield: %v", err)
	}
	return id
}

func (f *shieldRevocationFixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func (f *shieldRevocationFixture) shieldStatus(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT status FROM shields WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("shield status: %v", err)
	}
	return s
}

func TestRevokeShield_RecordsTombstoneAndPropagationWorkAtomically(t *testing.T) {
	t.Skip("bug 3: demonstrates the gap; un-skip with the fix")
	f := newShieldRevocationFixture(t)
	id := f.seedShield(t, "active")

	if err := shield.Revoke(f.ctx, txPool{f.pool}, f.wsID, id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := f.shieldStatus(t, id); got != "revoked" {
		t.Fatalf("status = %q, want revoked", got)
	}
	if n := f.count(t, `SELECT count(*) FROM shield_revocations WHERE shield_id = $1 AND tenant_id = $2`, id, f.wsID); n != 1 {
		t.Fatalf("tombstones = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM shield_revocation_outbox WHERE shield_id = $1 AND tenant_id = $2`, id, f.wsID); n != 1 {
		t.Fatalf("outbox entries = %d, want 1", n)
	}
}

// The revocation must not commit unless its propagation work is recorded.
func TestRevokeShield_DoesNotCommitWithoutPropagationWork(t *testing.T) {
	t.Skip("bug 3: demonstrates the gap; un-skip with the fix")
	f := newShieldRevocationFixture(t)
	id := f.seedShield(t, "active")
	// Make recording the propagation work fail.
	for _, sql := range []string{
		`CREATE FUNCTION fail_outbox() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'outbox unavailable'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER fail_outbox BEFORE INSERT ON shield_revocation_outbox FOR EACH ROW EXECUTE FUNCTION fail_outbox()`,
	} {
		if _, err := f.pool.Exec(f.ctx, sql); err != nil {
			t.Fatalf("install failure trigger: %v", err)
		}
	}

	if err := shield.Revoke(f.ctx, txPool{f.pool}, f.wsID, id); err == nil {
		t.Fatal("revoke succeeded although its propagation work could not be recorded")
	}
	if got := f.shieldStatus(t, id); got != "active" {
		t.Fatalf("status = %q: the revocation committed without its propagation work", got)
	}
	if n := f.count(t, `SELECT count(*) FROM shield_revocations WHERE shield_id = $1`, id); n != 0 {
		t.Fatalf("tombstone committed without its propagation work")
	}
}

func TestRevokeShield_IsTerminalAndIdempotent(t *testing.T) {
	t.Skip("bug 3: demonstrates the gap; un-skip with the fix")
	f := newShieldRevocationFixture(t)
	id := f.seedShield(t, "disconnected")
	if err := shield.Revoke(f.ctx, txPool{f.pool}, f.wsID, id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := shield.Revoke(f.ctx, txPool{f.pool}, f.wsID, id); !errors.Is(err, shield.ErrNotRevocable) {
		t.Fatalf("second revoke: want ErrNotRevocable, got %v", err)
	}
	other := newShieldRevocationFixture(t)
	if err := shield.Revoke(f.ctx, txPool{f.pool}, other.wsID, id); !errors.Is(err, shield.ErrNotRevocable) {
		t.Fatalf("revoke from another tenant: want ErrNotRevocable, got %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM shield_revocations WHERE shield_id = $1`, id); n != 1 {
		t.Fatalf("tombstones = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM shield_revocation_outbox WHERE shield_id = $1`, id); n != 1 {
		t.Fatalf("outbox entries = %d, want 1", n)
	}
}

// Deleting the shields row — directly, or by cascade from its connector —
// must neither erase the revocation nor free the identity for reuse.
func TestRevokedShieldIdentitySurvivesDeletion(t *testing.T) {
	t.Skip("bug 3: demonstrates the gap; un-skip with the fix")
	f := newShieldRevocationFixture(t)
	deleted := f.seedShield(t, "active")
	cascaded := f.seedShield(t, "active")
	for _, id := range []string{deleted, cascaded} {
		if err := shield.Revoke(f.ctx, txPool{f.pool}, f.wsID, id); err != nil {
			t.Fatalf("revoke: %v", err)
		}
	}
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM shields WHERE id = $1`, deleted); err != nil {
		t.Fatalf("delete shield: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM connectors WHERE id = $1`, f.connID); err != nil {
		t.Fatalf("delete connector: %v", err)
	}

	got, err := shield.RevokedShieldIDs(f.ctx, f.pool, f.wsID)
	if err != nil {
		t.Fatalf("revoked ids: %v", err)
	}
	want := []string{deleted, cascaded}
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("revoked ids after deletion = %v, want %v", got, want)
	}

	connID := seedConnector(t, f.pool, f.ctx, f.wsID, f.rnID, f.td, "active", nil)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO shields (id, tenant_id, remote_network_id, connector_id, name, status)
		 VALUES ($1, $2, $3, $4, 'reuse', 'pending')`,
		deleted, f.wsID, f.rnID, connID,
	); err == nil {
		t.Fatal("a revoked shield identity was reused")
	}
}

// revocationNotifier records NotifyPolicyChange calls and can fail them.
type revocationNotifier struct {
	mu      sync.Mutex
	tenants []string
	fail    error
}

func (n *revocationNotifier) NotifyPolicyChange(_ context.Context, workspaceID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fail != nil {
		return n.fail
	}
	n.tenants = append(n.tenants, workspaceID)
	return nil
}

func (n *revocationNotifier) notified() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.tenants...)
}

// drainRevocationLists returns every ShieldRevocationList queued for c.
func drainRevocationLists(c *connectorStreamClient) [][]string {
	var lists [][]string
	for {
		select {
		case msg := <-c.outbound:
			if l := msg.GetShieldRevocations(); l != nil {
				lists = append(lists, l.GetRevokedShieldIds())
			}
		default:
			return lists
		}
	}
}

func TestShieldRevocationDispatcher_DeliversThenClearsTheOutbox(t *testing.T) {
	t.Skip("bug 3: demonstrates the gap; un-skip with the fix")
	f := newShieldRevocationFixture(t)
	earlier := f.seedShield(t, "active")
	id := f.seedShield(t, "active")
	for _, s := range []string{earlier, id} {
		if err := shield.Revoke(f.ctx, txPool{f.pool}, f.wsID, s); err != nil {
			t.Fatalf("revoke: %v", err)
		}
	}
	// A connector of this workspace is connected, one of another is too.
	registry := NewConnectorRegistry()
	mine := testClient(f.connID, f.wsID)
	theirs := testClient("other-connector", "other-workspace")
	registry.add(mine.connectorID, mine)
	registry.add(theirs.connectorID, theirs)
	n := &revocationNotifier{}

	d := &ShieldRevocationDispatcher{Pool: f.pool, Registry: registry, PolicyNotifier: n}
	if err := d.ProcessOnce(f.ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	want := []string{earlier, id}
	sort.Strings(want)
	lists := drainRevocationLists(mine)
	if len(lists) == 0 || fmt.Sprint(lists[len(lists)-1]) != fmt.Sprint(want) {
		t.Fatalf("connector received %v, want the complete list %v", lists, want)
	}
	if got := drainRevocationLists(theirs); len(got) != 0 {
		t.Fatalf("another workspace's connector received %v", got)
	}
	if got := n.notified(); len(got) == 0 || got[0] != f.wsID {
		t.Fatalf("policy invalidated for %v, want %s", got, f.wsID)
	}
	if left := f.count(t, `SELECT count(*) FROM shield_revocation_outbox`); left != 0 {
		t.Fatalf("outbox not cleared: %d left", left)
	}
}

// Failed propagation keeps the work for a retry; a dispatcher started after a
// crash finishes work left by the previous process.
func TestShieldRevocationDispatcher_RetriesUntilDelivered(t *testing.T) {
	t.Skip("bug 3: demonstrates the gap; un-skip with the fix")
	f := newShieldRevocationFixture(t)
	id := f.seedShield(t, "active")
	if err := shield.Revoke(f.ctx, txPool{f.pool}, f.wsID, id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	registry := NewConnectorRegistry()
	c := testClient(f.connID, f.wsID)
	registry.add(c.connectorID, c)

	// 1. The policy invalidation fails: the work stays, with the error recorded.
	failing := &revocationNotifier{fail: errors.New("notifier down")}
	d := &ShieldRevocationDispatcher{Pool: f.pool, Registry: registry, PolicyNotifier: failing}
	_ = d.ProcessOnce(f.ctx)
	if n := f.count(t, `SELECT count(*) FROM shield_revocation_outbox WHERE attempts = 1 AND last_error IS NOT NULL`); n != 1 {
		t.Fatalf("failed delivery must stay queued with attempts=1 and last_error set (%d rows)", n)
	}

	// 2. The connector's send queue is full: the push fails, the work stays.
	for len(c.outbound) < cap(c.outbound) {
		c.outbound <- &pb.ConnectorControlMessage{}
	}
	d = &ShieldRevocationDispatcher{Pool: f.pool, Registry: registry, PolicyNotifier: &revocationNotifier{}}
	_ = d.ProcessOnce(f.ctx)
	if n := f.count(t, `SELECT count(*) FROM shield_revocation_outbox`); n != 1 {
		t.Fatalf("undelivered push must stay queued (%d rows)", n)
	}
	for len(c.outbound) > 0 {
		<-c.outbound
	}

	// 3. A fresh dispatcher (the process restarted) delivers it.
	d = &ShieldRevocationDispatcher{Pool: f.pool, Registry: registry, PolicyNotifier: &revocationNotifier{}}
	if err := d.ProcessOnce(f.ctx); err != nil {
		t.Fatalf("process after restart: %v", err)
	}
	if lists := drainRevocationLists(c); len(lists) == 0 || fmt.Sprint(lists[len(lists)-1]) != fmt.Sprint([]string{id}) {
		t.Fatalf("connector received %v after retry", lists)
	}
	if n := f.count(t, `SELECT count(*) FROM shield_revocation_outbox`); n != 0 {
		t.Fatalf("outbox not cleared after delivery (%d rows)", n)
	}
}

// revocationRecordingStream is a held Control stream that records what the
// controller sends.
type revocationRecordingStream struct {
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	sent   []*pb.ConnectorControlMessage
}

func (s *revocationRecordingStream) close()                       { s.once.Do(func() { close(s.closed) }) }
func (s *revocationRecordingStream) SetHeader(metadata.MD) error  { return nil }
func (s *revocationRecordingStream) SendHeader(metadata.MD) error { return nil }
func (s *revocationRecordingStream) SetTrailer(metadata.MD)       {}
func (s *revocationRecordingStream) Context() context.Context     { return s.ctx }
func (s *revocationRecordingStream) SendMsg(any) error            { return nil }
func (s *revocationRecordingStream) RecvMsg(any) error            { <-s.closed; return io.EOF }
func (s *revocationRecordingStream) Send(m *pb.ConnectorControlMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}
func (s *revocationRecordingStream) Recv() (*pb.ConnectorControlMessage, error) {
	<-s.closed
	return nil, io.EOF
}

func (s *revocationRecordingStream) revocationLists() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]string
	for _, m := range s.sent {
		if l := m.GetShieldRevocations(); l != nil {
			out = append(out, l.GetRevokedShieldIds())
		}
	}
	return out
}

func (f *shieldRevocationFixture) openControl(t *testing.T, h *EnrollmentHandler) (*revocationRecordingStream, chan error) {
	t.Helper()
	sctx, cancel := context.WithCancel(spiffe.WithIdentity(f.ctx,
		fmt.Sprintf("spiffe://%s/connector/%s", f.td, f.connID), "connector", f.connID, f.td))
	t.Cleanup(cancel)
	s := &revocationRecordingStream{ctx: sctx, closed: make(chan struct{})}
	t.Cleanup(s.close)
	done := make(chan error, 1)
	go func() { done <- h.Control(s) }()
	return s, done
}

func TestControl_SendsTheAuthoritativeRevocationListOnConnect(t *testing.T) {
	t.Skip("bug 3: demonstrates the gap; un-skip with the fix")
	f := newShieldRevocationFixture(t)
	revoked := f.seedShield(t, "active")
	_ = f.seedShield(t, "active") // live, must not be listed
	if err := shield.Revoke(f.ctx, txPool{f.pool}, f.wsID, revoked); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	other := newShieldRevocationFixture(t)
	if err := shield.Revoke(other.ctx, txPool{other.pool}, other.wsID, other.seedShield(t, "active")); err != nil {
		t.Fatalf("revoke other: %v", err)
	}

	h := &EnrollmentHandler{Pool: f.pool, Registry: NewConnectorRegistry()}
	s, _ := f.openControl(t, h)
	deadline := time.Now().Add(5 * time.Second)
	for len(s.revocationLists()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	lists := s.revocationLists()
	if len(lists) == 0 {
		t.Fatal("no ShieldRevocationList sent when the Control stream opened")
	}
	if fmt.Sprint(lists[0]) != fmt.Sprint([]string{revoked}) {
		t.Fatalf("initial list = %v, want exactly [%s]", lists[0], revoked)
	}
}

// If the controller cannot read the revocation state it must close the
// stream, so the connector keeps refusing shields, rather than leave a
// connected connector without its authoritative list.
func TestControl_ClosesTheStreamWhenRevocationsCannotBeRead(t *testing.T) {
	t.Skip("bug 3: demonstrates the gap; un-skip with the fix")
	f := newShieldRevocationFixture(t)
	if _, err := f.pool.Exec(f.ctx, `ALTER TABLE shield_revocations RENAME TO shield_revocations_gone`); err != nil {
		t.Fatalf("break revocation table: %v", err)
	}
	h := &EnrollmentHandler{Pool: f.pool, Registry: NewConnectorRegistry()}
	_, done := f.openControl(t, h)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Control returned nil although the revocation list could not be sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Control kept the stream open without an authoritative revocation list")
	}
}
