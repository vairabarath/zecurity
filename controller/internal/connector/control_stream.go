package connector

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/discovery"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/resource"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ConnectorRegistry tracks active bidirectional Control streams.
// The resolver calls PushResourceInstruction to deliver instructions in real time.
// If the connector is offline, the instruction stays in DB and is delivered on reconnect.
type ConnectorRegistry struct {
	mu      sync.Mutex
	clients map[string]*connectorStreamClient // keyed by connector_id
}

// NewConnectorRegistry creates an empty registry.
func NewConnectorRegistry() *ConnectorRegistry {
	return &ConnectorRegistry{clients: make(map[string]*connectorStreamClient)}
}

// connectorSendQueueSize bounds a connector's outbound mailbox. A wedged connector
// (one that stops draining its stream) fills this, after which sends fail fast
// rather than blocking the caller; the dropped message is recovered by reconnect
// replay or the Phase 3 reconciler. Sized for normal bursts (snapshot + acks + ACL).
const connectorSendQueueSize = 128

type connectorStreamClient struct {
	stream      pb.ConnectorService_ControlServer
	outbound    chan *pb.ConnectorControlMessage
	connectorID string
	tenantID    string
}

// send enqueues a message for the writer goroutine. It never blocks the caller: if
// the mailbox is full (connector not draining) it fails fast, so a wedged connector
// can't stall a GraphQL resolver or the reconciler on the socket write. gRPC permits
// only one concurrent Send, which runWriter (the sole sender) guarantees.
func (c *connectorStreamClient) send(msg *pb.ConnectorControlMessage) error {
	select {
	case c.outbound <- msg:
		return nil
	default:
		return fmt.Errorf("connector %s send queue full", c.connectorID)
	}
}

// runWriter is the only goroutine that calls stream.Send. It drains the outbound
// mailbox until the stream context is cancelled (connector disconnect / handler
// exit) or a Send fails (broken stream) — in which case the recv loop also observes
// the error and tears the connection down. A blocking Send on a wedged connector
// blocks only this goroutine; callers have already failed fast in send().
func (c *connectorStreamClient) runWriter(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.outbound:
			if err := c.stream.Send(msg); err != nil {
				log.Printf("control stream: send to connector %s failed: %v", c.connectorID, err)
				return
			}
		}
	}
}

func (r *ConnectorRegistry) add(connectorID string, c *connectorStreamClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[connectorID] = c
}

func (r *ConnectorRegistry) remove(connectorID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, connectorID)
}

func (r *ConnectorRegistry) get(connectorID string) *connectorStreamClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clients[connectorID]
}

// buildSnapshotMsg builds the authoritative desired-state snapshot message for
// one shield (ADR-004 Phase 2). The desired set and its monotonic generation come
// from resource.BuildShieldSnapshot — the generation is a per-shield counter
// bumped only when the desired content actually changes, so the shield's
// `generation <= last` gate dedups unchanged re-pushes and resolves out-of-order
// deliveries (see F11). It is NOT wall-clock and not derived in this layer.
func buildSnapshotMsg(ctx context.Context, db *pgxpool.Pool, shieldID string) (*pb.ConnectorControlMessage, error) {
	snap, err := resource.BuildShieldSnapshot(ctx, db, shieldID)
	if err != nil {
		return nil, fmt.Errorf("buildSnapshotMsg: %w", err)
	}

	resources := make([]*shieldpb.ResourceInstruction, 0, len(snap.Resources))
	for _, row := range snap.Resources {
		resources = append(resources, &shieldpb.ResourceInstruction{
			ResourceId: row.ID,
			Host:       row.Host,
			Protocol:   row.Protocol,
			PortFrom:   int32(row.PortFrom),
			PortTo:     int32(row.PortTo),
		})
	}

	return &pb.ConnectorControlMessage{
		Body: &pb.ConnectorControlMessage_ResourceSnapshots{
			ResourceSnapshots: &pb.ResourceSnapshotBatch{
				ShieldSnapshots: map[string]*shieldpb.ResourceSnapshot{
					shieldID: {Resources: resources, Generation: snap.Generation},
				},
			},
		},
	}, nil
}

// PushSnapshotForShield refreshes the connector's cached desired-state snapshot
// for a shield. Called after every mutation that changes the desired set —
// keeping the cache fresh is what makes the connector's replay-on-shield-connect
// safe (a stale snapshot would wipe newer rules). Offline connector is fine:
// snapshots are re-sent for every shield on connector connect.
func PushSnapshotForShield(ctx context.Context, db *pgxpool.Pool, r *ConnectorRegistry, connectorID, shieldID string) {
	if connectorID == "" {
		return
	}

	c := r.get(connectorID)
	if c == nil {
		return
	}
	msg, err := buildSnapshotMsg(ctx, db, shieldID)
	if err != nil {
		log.Printf("control stream: build snapshot for shield %s: %v", shieldID, err)
		return
	}
	if err := c.send(msg); err != nil {
		log.Printf("control stream: push snapshot to connector %s: %v", connectorID, err)
	}
}

// PushInstruction builds and delivers a single resource instruction from a Row to
// the connector managing that shield. Safe to call even when the connector is offline —
// the instruction is already written to DB by the caller and will be delivered on reconnect.
func (r *ConnectorRegistry) PushInstruction(row *resource.Row) {
	if row.ConnectorID == "" {
		return
	}
	instr := &shieldpb.ResourceInstruction{
		ResourceId: row.ID,
		Host:       row.Host,
		Protocol:   row.Protocol,
		PortFrom:   int32(row.PortFrom),
		PortTo:     int32(row.PortTo),
		Action:     row.PendingAction,
	}
	_ = r.PushResourceInstruction(row.ConnectorID, row.ShieldID, []*shieldpb.ResourceInstruction{instr})
}

// PushResourceInstruction delivers a resource instruction to the connector that
// manages shieldID. Returns an error only if the connector is not connected; the
// instruction remains in DB either way and will be delivered on reconnect.
func (r *ConnectorRegistry) PushResourceInstruction(
	connectorID, shieldID string,
	instructions []*shieldpb.ResourceInstruction,
) error {
	c := r.get(connectorID)
	if c == nil {
		return nil // offline — instruction already written to DB by caller
	}
	msg := &pb.ConnectorControlMessage{
		Body: &pb.ConnectorControlMessage_ResourceInstructions{
			ResourceInstructions: &pb.ResourceInstructionBatch{
				ShieldResources: map[string]*pb.ShieldResourceInstructions{
					shieldID: {Instructions: instructions},
				},
			},
		},
	}
	if err := c.send(msg); err != nil {
		log.Printf("control stream: push instruction to connector %s: %v", connectorID, err)
		return err
	}
	return nil
}

// PushScanCommand delivers a ScanCommand to a connected connector.
// Returns an error if the connector is not currently connected.
func (r *ConnectorRegistry) PushScanCommand(connectorID string, msg *pb.ConnectorControlMessage) error {
	c := r.get(connectorID)
	if c == nil {
		return fmt.Errorf("connector %s is not connected", connectorID)
	}
	return c.send(msg)
}

// Control implements ConnectorService.Control — the persistent bidirectional stream.
func (h *EnrollmentHandler) Control(stream pb.ConnectorService_ControlServer) error {
	ctx := stream.Context()

	println("=== STDOUT TEST FROM CONTROLLER ===")
	log.Printf("=== CONTROLLER: Control() handler invoked for connector ===")

	trustDomain := TrustDomainFromContext(ctx)
	role := SPIFFERoleFromContext(ctx)
	connectorID := SPIFFEEntityIDFromContext(ctx)

	if role != appmeta.SPIFFERoleConnector {
		return status.Errorf(codes.PermissionDenied, "expected role %q, got %q", appmeta.SPIFFERoleConnector, role)
	}

	var connStatus, tenantID string
	if err := h.Pool.QueryRow(ctx,
		`SELECT status, tenant_id FROM connectors WHERE id = $1 AND trust_domain = $2`,
		connectorID, trustDomain,
	).Scan(&connStatus, &tenantID); err != nil {
		return status.Errorf(codes.NotFound, "connector not found: %v", err)
	}
	if connStatus == "revoked" {
		return status.Error(codes.PermissionDenied, "connector is revoked")
	}

	client := &connectorStreamClient{
		stream:      stream,
		outbound:    make(chan *pb.ConnectorControlMessage, connectorSendQueueSize),
		connectorID: connectorID,
		tenantID:    tenantID,
	}
	h.Registry.add(connectorID, client)
	defer h.Registry.remove(connectorID)

	// A single writer goroutine owns stream.Send; every send goes through
	// client.send (enqueue). This decouples resolvers and the reconciler from the
	// socket write, so a wedged connector can block only this goroutine — which
	// unblocks when ctx is cancelled (handler returns) or the Send fails.
	go client.runWriter(ctx)

	_, _ = h.Pool.Exec(ctx,
		`UPDATE connectors SET status = 'active', last_heartbeat_at = NOW(), updated_at = NOW() WHERE id = $1`,
		connectorID,
	)
	defer func() {
		// Use background context — stream context is already cancelled at this point.
		_, _ = h.Pool.Exec(context.Background(),
			`UPDATE connectors SET status = 'disconnected', updated_at = NOW() WHERE id = $1`,
			connectorID,
		)
		log.Printf("control stream: connector %s disconnected", connectorID)
	}()

	log.Printf("control stream: connector %s connected", connectorID)

	// Flush gRPC headers by enqueuing an initial Ping (the writer sends it first,
	// resolving the client-side .control().await immediately). Enqueue can't fail on
	// a fresh empty mailbox; if it ever did, the recv loop would surface the break.
	if err := client.send(&pb.ConnectorControlMessage{
		Body: &pb.ConnectorControlMessage_Ping{
			Ping: &shieldpb.Ping{TimestampUnix: time.Now().Unix()},
		},
	}); err != nil {
		log.Printf("control stream: initial ping enqueue to connector %s failed: %v", connectorID, err)
	}

	// Deliver any instructions that queued while the connector was offline.
	log.Printf("control stream: pushing pending instructions for connector %s", connectorID)
	if err := h.pushPendingInstructions(ctx, client); err != nil {
		log.Printf("control stream: push pending for connector %s: %v", connectorID, err)
	}
	log.Printf("control stream: entering message loop for connector %s", connectorID)

	for {
		log.Printf("control stream: waiting for message from connector %s", connectorID)
		msg, err := stream.Recv()
		if err == io.EOF {
			log.Printf("control stream: connector %s closed stream (EOF)", connectorID)
			return nil
		}
		if err != nil {
			log.Printf("control stream: connector %s stream error: %v", connectorID, err)
			return err
		}

		log.Printf("control stream: received message from connector %s, body type: %T", connectorID, msg.Body)

		switch msg.Body.(type) {
		case nil:
			log.Printf("control stream: connector %s body is NIL", connectorID)
		case *pb.ConnectorControlMessage_ConnectorHealth:
			h.handleConnectorHealth(ctx, client, msg.Body.(*pb.ConnectorControlMessage_ConnectorHealth).ConnectorHealth)
		case *pb.ConnectorControlMessage_ShieldStatus:
			h.handleShieldStatus(ctx, connectorID, msg.Body.(*pb.ConnectorControlMessage_ShieldStatus).ShieldStatus)
		case *pb.ConnectorControlMessage_ResourceAcks:
			h.handleResourceAcks(ctx, tenantID, msg.Body.(*pb.ConnectorControlMessage_ResourceAcks).ResourceAcks)
		case *pb.ConnectorControlMessage_ShieldDiscovery:
			batch := msg.Body.(*pb.ConnectorControlMessage_ShieldDiscovery)
			log.Printf("control stream: connector %s case ShieldDiscovery REPORTS=%d", connectorID, len(batch.ShieldDiscovery.Reports))
			h.handleShieldDiscoveryBatch(ctx, batch.ShieldDiscovery)
		case *pb.ConnectorControlMessage_ScanReport:
			rep := msg.Body.(*pb.ConnectorControlMessage_ScanReport)
			log.Printf("control stream: connector %s case ScanReport request_id=%s", connectorID, rep.ScanReport.RequestId)
			h.handleScanReport(ctx, connectorID, rep.ScanReport)
		case *pb.ConnectorControlMessage_Pong:
			log.Printf("control stream: connector %s case Pong", connectorID)
		case *pb.ConnectorControlMessage_ConnectorLog:
			entry := msg.Body.(*pb.ConnectorControlMessage_ConnectorLog).ConnectorLog
			h.handleConnectorLog(ctx, tenantID, connectorID, entry)
		case *pb.ConnectorControlMessage_ResourceState:
			h.handleResourceState(ctx, client, msg.Body.(*pb.ConnectorControlMessage_ResourceState).ResourceState)
		default:
			log.Printf("control stream: connector %s UNKNOWN case: %T", connectorID, msg.Body)
		}
	}
}

// pushPendingInstructions sends any DB-pending instructions to a freshly connected connector.
func (h *EnrollmentHandler) pushPendingInstructions(ctx context.Context, client *connectorStreamClient) error {
	rows, err := h.Pool.Query(ctx,
		`SELECT id FROM shields WHERE connector_id = $1 AND status NOT IN ('revoked', 'deleted')`,
		client.connectorID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var shieldID string
		if err := rows.Scan(&shieldID); err != nil {
			continue
		}

		// ADR-004 Phase 2: refresh this shield's cached desired-state snapshot.
		// Sent for EVERY shield (even with nothing pending) so a shield that
		// reboots later gets re-protected from the connector's cache.
		if msg, err := buildSnapshotMsg(ctx, h.Pool, shieldID); err == nil {
			if err := client.send(msg); err != nil {
				log.Printf("control stream: send snapshot for shield %s: %v", shieldID, err)
			}
		} else {
			log.Printf("control stream: build snapshot for shield %s: %v", shieldID, err)
		}

		// The snapshot above is the authoritative APPLY path: it enforces the full
		// desired set (protected/failed/protecting-apply) and acks each resource, so
		// protect completions still happen on reconnect. It only drops removed
		// resources by OMISSION, without an ack — so explicit 'remove' instructions
		// must still be delivered for tombstones ('deleting') and in-flight unprotects
		// ('protecting'/remove): handle_remove emits the 'unprotected' ack that reaps
		// a tombstone immediately (the state-report reconciler is the slower backstop).
		// 'apply' instructions are intentionally NOT re-sent — the snapshot already
		// covers them — avoiding a redundant second chain rebuild + duplicate ack.
		pending, err := resource.GetPendingForShield(ctx, h.Pool, shieldID)
		if err != nil {
			continue
		}
		instrs := make([]*shieldpb.ResourceInstruction, 0, len(pending))
		for _, r := range pending {
			if r.PendingAction != "remove" {
				continue // applies are delivered by the snapshot above
			}
			instrs = append(instrs, &shieldpb.ResourceInstruction{
				ResourceId: r.ID,
				Host:       r.Host,
				Protocol:   r.Protocol,
				PortFrom:   int32(r.PortFrom),
				PortTo:     int32(r.PortTo),
				Action:     r.PendingAction,
			})
		}
		if len(instrs) == 0 {
			continue
		}
		msg := &pb.ConnectorControlMessage{
			Body: &pb.ConnectorControlMessage_ResourceInstructions{
				ResourceInstructions: &pb.ResourceInstructionBatch{
					ShieldResources: map[string]*pb.ShieldResourceInstructions{
						shieldID: {Instructions: instrs},
					},
				},
			},
		}
		if err := client.send(msg); err != nil {
			log.Printf("control stream: send pending instructions to shield %s: %v", shieldID, err)
		}
	}
	return rows.Err()
}

func (h *EnrollmentHandler) handleConnectorHealth(ctx context.Context, client *connectorStreamClient, r *pb.ConnectorHealthReport) {
	connectorID := client.connectorID
	log.Printf("control stream: received health report connector=%s version=%s hostname=%s lan_addr=%s acl_version=%d", connectorID, r.Version, r.Hostname, r.LanAddr, r.AclVersion)
	_, err := h.Pool.Exec(ctx,
		`UPDATE connectors
		    SET version           = $1,
		        hostname          = $2,
		        public_ip         = $3,
		        lan_addr          = NULLIF($4, ''),
		        last_heartbeat_at = NOW(),
		        updated_at        = NOW()
		  WHERE id = $5`,
		r.Version, r.Hostname, r.PublicIp, r.LanAddr, connectorID,
	)
	if err != nil {
		log.Printf("control stream: update connector health %s: %v", connectorID, err)
	}
	if err := h.pushACLSnapshot(ctx, client, r.AclVersion); err != nil {
		log.Printf("control stream: push ACL snapshot to connector %s: %v", connectorID, err)
	}
}

func (h *EnrollmentHandler) pushACLSnapshot(ctx context.Context, client *connectorStreamClient, connectorVersion uint64) error {
	if h.PolicyStore == nil || h.PolicyCache == nil || h.PolicyNotifier == nil {
		return nil
	}

	snap, ok := h.PolicyCache.Get(client.tenantID)
	if !ok {
		compiled, err := policy.CompileACLSnapshot(ctx, h.PolicyStore, h.PolicyNotifier, h.Pool, client.tenantID)
		if err != nil {
			return fmt.Errorf("compile ACL snapshot: %w", err)
		}
		h.PolicyCache.Set(client.tenantID, compiled)
		snap = compiled
	}

	if connectorVersion == snap.Version {
		log.Printf("control stream: connector ACL already current connector=%s version=%d entries=%d", client.connectorID, snap.Version, len(snap.Entries))
		return nil
	}

	if err := client.send(&pb.ConnectorControlMessage{
		Body: &pb.ConnectorControlMessage_AclSnapshot{
			AclSnapshot: snap,
		},
	}); err != nil {
		return err
	}
	log.Printf("control stream: pushed ACL snapshot connector=%s version=%d entries=%d", client.connectorID, snap.Version, len(snap.Entries))
	return nil
}

func (h *EnrollmentHandler) handleShieldStatus(ctx context.Context, connectorID string, batch *pb.ShieldStatusBatch) {
	for _, s := range batch.Shields {
		if err := h.ShieldSvc.UpdateShieldHealth(
			ctx, s.ShieldId, connectorID, s.Status, s.Version, s.LanIp, s.LastSeenUnix,
		); err != nil {
			log.Printf("control stream: update shield health %s: %v", s.ShieldId, err)
		}
	}
}

func (h *EnrollmentHandler) handleResourceAcks(ctx context.Context, tenantID string, batch *pb.ResourceAckBatch) {
	for _, ack := range batch.Acks {
		if err := resource.RecordAck(
			ctx, h.Pool, tenantID,
			ack.ResourceId, ack.Status, ack.Error,
			ack.VerifiedAt, ack.PortReachable,
		); err != nil {
			log.Printf("control stream: record ack resource_id=%s: %v", ack.ResourceId, err)
		}
	}
}

// StreamSPIFFEInterceptor wraps the stream SPIFFE identity injection for Control RPCs.
// The gRPC server must register this as a StreamInterceptor alongside the UnaryInterceptor.
func StreamSPIFFEInterceptor(validator TrustDomainValidator, store WorkspaceStore) grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		ctx := ss.Context()

		p, err := peerFromContext(ctx)
		if err != nil {
			return status.Error(codes.Unauthenticated, err.Error())
		}

		leaf := p[0]
		trustDomain, role, entityID, parseErr := parseSPIFFEID(leaf)
		if parseErr != nil {
			return status.Errorf(codes.Unauthenticated, "invalid SPIFFE ID: %v", parseErr)
		}

		if !validator(ctx, trustDomain) {
			return status.Errorf(codes.PermissionDenied, "trust domain %q not accepted", trustDomain)
		}

		if role == appmeta.SPIFFERoleConnector {
			if err := verifyConnectorCertificate(ctx, store, trustDomain, leaf); err != nil {
				return status.Errorf(codes.Unauthenticated, "connector certificate verification failed: %v", err)
			}
		}

		spiffeID := "spiffe://" + trustDomain + "/" + role + "/" + entityID
		ctx = spiffe.WithIdentity(ctx, spiffeID, role, entityID, trustDomain)

		return handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
	}
}

// wrappedStream injects the enriched context into a gRPC server stream.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// peerFromContext extracts the leaf certificate from the gRPC peer TLS state.
func peerFromContext(ctx context.Context) ([]*x509.Certificate, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer info")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no TLS credentials")
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "no client certificate")
	}
	return tlsInfo.State.PeerCertificates, nil
}

func (h *EnrollmentHandler) handleShieldDiscoveryBatch(ctx context.Context, batch *pb.ShieldDiscoveryBatch) {
	for _, entry := range batch.Reports {
		shieldID := entry.ShieldId
		r := entry.Report
		if r == nil {
			continue
		}

		log.Printf("discovery: shield %s full_sync=%v added=%d removed=%d", shieldID, r.FullSync, len(r.Added), len(r.Removed))
		if r.FullSync {
			var services []discovery.DiscoveredService
			for _, svc := range r.Added {
				services = append(services, protoToDiscoveredService(shieldID, svc))
			}
			log.Printf("discovery: calling ReplaceDiscoveredServices shield=%s services=%d", shieldID, len(services))
			if err := discovery.ReplaceDiscoveredServices(ctx, h.Pool, shieldID, services); err != nil {
				log.Printf("discovery: replace FAILED for shield %s: %v", shieldID, err)
			} else {
				log.Printf("discovery: replace OK for shield %s services=%d", shieldID, len(services))
			}
		} else {
			var added, removed []discovery.DiscoveredService
			for _, svc := range r.Added {
				added = append(added, protoToDiscoveredService(shieldID, svc))
			}
			for _, svc := range r.Removed {
				removed = append(removed, discovery.DiscoveredService{
					Protocol: svc.Protocol,
					Port:     int(svc.Port),
				})
			}
			log.Printf("discovery: calling UpsertDiscoveredServices shield=%s added=%d removed=%d", shieldID, len(added), len(removed))
			if err := discovery.UpsertDiscoveredServices(ctx, h.Pool, shieldID, added, removed); err != nil {
				log.Printf("discovery: upsert FAILED for shield %s: %v", shieldID, err)
			} else {
				log.Printf("discovery: upsert OK for shield %s added=%d removed=%d", shieldID, len(added), len(removed))
			}
		}
	}
}

func (h *EnrollmentHandler) handleScanReport(ctx context.Context, connectorID string, rep *pb.ScanReport) {
	var results []discovery.ScanResult
	for _, r := range rep.Results {
		results = append(results, discovery.ScanResult{
			RequestID:   rep.RequestId,
			ConnectorID: connectorID,
			IP:          r.Ip,
			Port:        int(r.Port),
			Protocol:    r.Protocol,
			ServiceName: r.ServiceName,
		})
	}
	if err := discovery.UpsertScanResults(ctx, h.Pool, connectorID, results); err != nil {
		log.Printf("discovery: scan upsert failed for request %s: %v", rep.RequestId, err)
	}
}

func (h *EnrollmentHandler) handleConnectorLog(ctx context.Context, tenantID, connectorID string, entry *pb.ConnectorLog) {
	_, err := h.Pool.Exec(ctx,
		`INSERT INTO connector_logs (workspace_id, connector_id, message)
		 VALUES ($1, $2, $3)`,
		tenantID, connectorID, entry.Message,
	)
	if err != nil {
		log.Printf("control stream: insert connector log connector=%s: %v", connectorID, err)
	}
}

func protoToDiscoveredService(shieldID string, svc *shieldpb.DiscoveredService) discovery.DiscoveredService {
	return discovery.DiscoveredService{
		ShieldID:    shieldID,
		Protocol:    svc.Protocol,
		Port:        int(svc.Port),
		BoundIP:     svc.BoundIp,
		ServiceName: svc.ServiceName,
	}
}
