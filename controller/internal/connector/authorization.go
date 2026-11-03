package connector

// Controller ↔ connector authorization contract, Phase A
// (.zecurity-obs/Sprint20/Fixes/connector-revocation/
// Connector-Revocation-Implementation-Design-2026-10-14.md §4.3, §4.5, D9–D11).
//
//   - AuthorizationAck{seq} is enqueued only after the revocation-guarded health
//     UPDATE returned a row and any required ACL push for that report was
//     enqueued ahead of it on the same FIFO stream.
//   - Authoritative connector states travel as a structured trailer marker
//     (ConnectorStateMarkerKey), attached only after a SUCCESSFUL lookup.
//     Human-readable status messages carry no security meaning.
//   - A database or internal failure is Unavailable with no marker: never
//     NotFound, never revoked, never unknown.
//   - ConnectorRevoked is delivered on every live Control stream of a revoked
//     connector, including a stream superseded by a make-before-break renewal
//     that the connector may still be reading.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ConnectorStateMarkerKey is the gRPC trailer key carrying the authoritative
// connector state on a terminal Control-stream status (D10). gRPC metadata
// keys are lower-case ASCII.
const ConnectorStateMarkerKey = "zecurity-connector-state"

// Marker values. Only these two exist; any other status is non-terminal.
const (
	ConnectorStateRevoked = "revoked" // REVOKED
	ConnectorStateUnknown = "unknown" // UNKNOWN_CONNECTOR
)

// connectorAuthState is the result of a SUCCESSFUL authoritative lookup.
// An undetermined state is expressed as a non-nil lookup error, never as one
// of these values.
type connectorAuthState int

const (
	connectorStateAuthorized connectorAuthState = iota + 1 // row present, not revoked
	connectorStateRevoked                                  // status='revoked' or revoked_at set, or a tombstone (F40)
	connectorStateAbsent                                   // no row and no tombstone for (id, trust_domain)
)

// connectorRecord is the row read by lookupConnector.
type connectorRecord struct {
	status    string
	tenantID  string
	revokedAt *time.Time
}

// lookupConnector is the single authoritative connector-state lookup. It
// distinguishes the three determined states from a failed query: err != nil
// means the state is UNKNOWN TO THE CONTROLLER (DB/pool/internal failure) and
// must be reported as Unavailable without a marker.
func (h *EnrollmentHandler) lookupConnector(ctx context.Context, connectorID, trustDomain string) (connectorAuthState, connectorRecord, error) {
	if h.lookupOverride != nil {
		return h.lookupOverride(ctx, connectorID, trustDomain)
	}
	var rec connectorRecord
	err := h.Pool.QueryRow(ctx,
		`SELECT status, tenant_id, revoked_at FROM connectors WHERE id = $1 AND trust_domain = $2`,
		connectorID, trustDomain,
	).Scan(&rec.status, &rec.tenantID, &rec.revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Sprint 20 Fix 13 (F40): a deleted connector whose identity was
		// revoked stays revoked (041 tombstone), not unknown. Only the
		// workspace with this trust domain counts.
		var tombstoned bool
		err = h.Pool.QueryRow(ctx,
			`SELECT EXISTS (
			     SELECT 1 FROM connector_revocations cr
			       JOIN workspaces w ON w.id = cr.tenant_id
			      WHERE cr.connector_id = $1 AND w.trust_domain = $2)`,
			connectorID, trustDomain,
		).Scan(&tombstoned)
		if err != nil {
			return 0, rec, err
		}
		if tombstoned {
			return connectorStateRevoked, rec, nil
		}
		return connectorStateAbsent, rec, nil
	}
	if err != nil {
		return 0, rec, err
	}
	if rec.status == "revoked" || rec.revokedAt != nil {
		return connectorStateRevoked, rec, nil
	}
	return connectorStateAuthorized, rec, nil
}

// authzRejection is a Control-stream termination decided by the authorization
// contract. marker is non-empty only for an authoritatively determined state.
type authzRejection struct {
	code   codes.Code
	msg    string
	marker string
}

func (r *authzRejection) Error() string { return r.msg }

func rejectRevoked() *authzRejection {
	return &authzRejection{code: codes.PermissionDenied, msg: "connector is revoked", marker: ConnectorStateRevoked}
}

func rejectUnknown() *authzRejection {
	return &authzRejection{code: codes.NotFound, msg: "connector not found", marker: ConnectorStateUnknown}
}

// rejectUnavailable never carries a marker: the state was not determined.
func rejectUnavailable() *authzRejection {
	return &authzRejection{code: codes.Unavailable, msg: "connector authorization state unavailable"}
}

// rejectMissingSeq: a health report without the authorization contract.
func rejectMissingSeq() *authzRejection {
	return &authzRejection{code: codes.FailedPrecondition, msg: "connector health report lacks heartbeat_seq (authorization contract required)"}
}

// rejectionFor maps a lookup outcome to the termination it requires. A
// lookup that succeeded and found an authorized row while the caller expected
// a refusal (a guarded UPDATE matched nothing) is inconsistent and therefore
// undetermined: Unavailable, no marker.
func rejectionFor(state connectorAuthState, err error) *authzRejection {
	if err != nil {
		return rejectUnavailable()
	}
	switch state {
	case connectorStateRevoked:
		return rejectRevoked()
	case connectorStateAbsent:
		return rejectUnknown()
	default:
		return rejectUnavailable()
	}
}

// classifyRefusal runs the authoritative follow-up lookup after a
// revocation-guarded statement matched no row (ErrNoRows conflates revoked
// with deleted, design §4.1) and returns the termination it implies.
func (h *EnrollmentHandler) classifyRefusal(ctx context.Context, client *connectorStreamClient, where string) *authzRejection {
	state, _, err := h.lookupConnector(ctx, client.connectorID, client.trustDomain)
	rej := rejectionFor(state, err)
	if err != nil {
		log.Printf("control stream: %s: connector %s follow-up state lookup failed (state undetermined, no marker): %v", where, client.connectorID, err)
	} else {
		log.Printf("control stream: %s: connector %s refused: code=%s marker=%q", where, client.connectorID, rej.code, rej.marker)
	}
	return rej
}

// rejectStream converts rej into the gRPC status the handler returns. The
// marker is set as trailer metadata on the stream before the status, so it
// survives both a trailers-only response and a mid-stream termination.
func rejectStream(stream grpc.ServerStream, rej *authzRejection) error {
	if rej.marker != "" {
		stream.SetTrailer(metadata.Pairs(ConnectorStateMarkerKey, rej.marker))
	}
	return status.Error(rej.code, rej.msg)
}

// errStreamRevoked: an ack was refused because ConnectorRevoked was already
// queued on this stream.
var errStreamRevoked = errors.New("connector stream carries a revocation")

// sendAuthorizationAck enqueues AuthorizationAck{seq}. It serialises with
// markRevoked on the same stream: once ConnectorRevoked was queued, no ack can
// follow it. The caller must already have passed every authorization check.
func (c *connectorStreamClient) sendAuthorizationAck(seq uint64) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.revoked {
		return errStreamRevoked
	}
	return c.send(&pb.ConnectorControlMessage{
		Body: &pb.ConnectorControlMessage_AuthorizationAck{
			AuthorizationAck: &pb.AuthorizationAck{HeartbeatSeq: seq},
		},
	})
}

// markRevoked records the revocation on this stream and queues
// ConnectorRevoked in the stream's one-slot terminal mailbox. The writer
// sends it ahead of anything still queued in outbound, so a full outbound
// mailbox cannot drop it. Reports whether this call queued it.
func (c *connectorStreamClient) markRevoked() bool {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.revoked {
		return false
	}
	c.revoked = true
	if c.terminal == nil {
		return false
	}
	select {
	case c.terminal <- &pb.ConnectorControlMessage{
		Body: &pb.ConnectorControlMessage_ConnectorRevoked{ConnectorRevoked: &pb.ConnectorRevoked{}},
	}:
		return true
	default:
		return false
	}
}

// trackLive / untrackLive maintain every live Control stream per connector,
// including streams superseded by a make-before-break renewal (which the
// registry's clients map no longer points at, but the connector may still be
// reading). Called under lifecycleMu by activateStream / retireStream.
func (r *ConnectorRegistry) trackLive(connectorID string, c *connectorStreamClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		r.live = make(map[string]map[*connectorStreamClient]struct{})
	}
	set := r.live[connectorID]
	if set == nil {
		set = make(map[*connectorStreamClient]struct{})
		r.live[connectorID] = set
	}
	set[c] = struct{}{}
}

func (r *ConnectorRegistry) untrackLive(connectorID string, c *connectorStreamClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.live[connectorID]
	delete(set, c)
	if len(set) == 0 {
		delete(r.live, connectorID)
	}
}

// SignalRevoked delivers ConnectorRevoked on every live Control stream of
// connectorID. Call it only AFTER the revoking UPDATE committed.
//
// It holds lifecycleMu, so it is atomic with respect to stream activation
// (guarded UPDATE + registration): an activation that ran before the revoke
// committed has registered its stream by the time this runs, and one that runs
// after sees the revoked row and is refused with the revoked marker. Every
// stream for this connector therefore either receives ConnectorRevoked or was
// never activated. Returns the number of streams it was queued on.
func (r *ConnectorRegistry) SignalRevoked(connectorID string) int {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	r.mu.RLock()
	streams := make([]*connectorStreamClient, 0, len(r.live[connectorID]))
	for c := range r.live[connectorID] {
		streams = append(streams, c)
	}
	r.mu.RUnlock()

	n := 0
	for _, c := range streams {
		if c.markRevoked() {
			n++
		}
	}
	log.Printf("control stream: connector %s revoked — ConnectorRevoked queued on %d live stream(s)", connectorID, n)
	return n
}

// authorizationOutcome is a short label for logs.
func describeRejection(rej *authzRejection) string {
	if rej == nil {
		return "none"
	}
	return fmt.Sprintf("code=%s marker=%q", rej.code, rej.marker)
}
