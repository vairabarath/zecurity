package resource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	pgx "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/apperr"
)

var ErrNoMatchingShield = errors.New("no active shield installed on resource host")

// ErrRemoteNetworkNotFound answers a remote network that doesn't exist or
// belongs to another workspace, with one user-safe message for both, so the
// response reveals nothing about other workspaces' networks.
var ErrRemoteNetworkNotFound = apperr.UserErrorf("remote network not found")

// ErrInvalidHost refuses a resource host that is neither a usable IPv4
// address nor a valid DNS name. The client handles IPv4 literals and, from
// PENDING-14, DNS names; anything else (IPv6, a port, a CIDR) used to be stored
// and could never be reached (PENDING-14 F0).
var ErrInvalidHost = apperr.UserErrorf("host must be an IPv4 address (such as 10.0.0.5) or a DNS name (such as db.internal.example.com); IPv6 is not supported")

// ErrNameNeedsTCP refuses a DNS-name resource whose protocol isn't TCP: the
// client resolves names to addresses it captures for TCP only (PENDING-14 F-1).
var ErrNameNeedsTCP = apperr.UserErrorf("a resource with a DNS name host must use protocol tcp")

// ErrNameProtectedMove refuses moving a protected DNS-name resource to another
// remote network: its shield was chosen by the admin, and a name has no LAN IP
// to find a shield in the new network by (PENDING-14 F-5).
var ErrNameProtectedMove = apperr.UserErrorf("a protected resource with a DNS name can't move to another remote network; unprotect it first")

// ErrNameCollision refuses a DNS-name resource whose name and ports overlap a
// resource with the same name in another remote network of the workspace: a
// client allowed both could not tell which network to reach (PENDING-14 F-6).
var ErrNameCollision = apperr.UserErrorf("another remote network in this workspace already has a resource with this DNS name on an overlapping port")

// HostKind says what a normalised resource host is.
type HostKind int

const (
	HostIPv4 HostKind = iota
	HostName
)

// ipv4HostPattern tells the two kinds of normalised host apart, in Go and in
// SQL (shieldHostSQL): a canonical IPv4 literal is digits and dots only, and a
// valid name always has a letter in its last label, so a name never matches.
const ipv4HostPattern = `^[0-9.]+$`

var ipv4HostRe = regexp.MustCompile(ipv4HostPattern)

// IsNameHost reports whether a stored (normalised) host is a DNS name.
func IsNameHost(host string) bool { return !ipv4HostRe.MatchString(host) }

// NormalizeHost validates a resource host and returns its canonical form and
// kind. Surrounding spaces are trimmed.
//
// IPv4: a dotted-quad literal, stored in canonical form. Refused: leading
// zeros, IPv6 (incl. IPv4-mapped "::ffff:a.b.c.d"), zone suffixes, and the
// unspecified, broadcast and multicast addresses, which never name one host.
//
// DNS name (PENDING-14): lowercased, one trailing dot dropped; at most 253
// characters; at least two labels; each label 1-63 letters, digits or hyphens,
// not starting or ending with a hyphen; the last label contains a letter (so a
// name can never be mistaken for an address). No wildcards (F-4).
func NormalizeHost(host string) (string, HostKind, error) {
	h := strings.TrimSpace(host)
	if a, err := netip.ParseAddr(h); err == nil {
		if !a.Is4() || a.IsUnspecified() || a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
			return "", 0, ErrInvalidHost
		}
		return a.String(), HostIPv4, nil
	}
	name := strings.ToLower(strings.TrimSuffix(h, "."))
	if len(name) == 0 || len(name) > 253 {
		return "", 0, ErrInvalidHost
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return "", 0, ErrInvalidHost
	}
	for _, l := range labels {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", 0, ErrInvalidHost
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", 0, ErrInvalidHost
			}
		}
	}
	if !strings.ContainsAny(labels[len(labels)-1], "abcdefghijklmnopqrstuvwxyz") {
		return "", 0, ErrInvalidHost
	}
	return name, HostName, nil
}

// shieldHostSQL is the host a shield is told to protect and acks back, for a
// query that has the resource as r and LEFT JOINs its bound shield as s
// (s.id = r.shield_id). An IPv4 resource's host is the shield's LAN IP already.
// A DNS-name resource is an alias for its shield's host (PENDING-14 F-5), so
// the shield gets its own LAN IP, never the name; the shield then sees an
// ordinary IP resource. Without a shield it falls back to the name, which no
// shield accepts.
const shieldHostSQL = `CASE WHEN r.host ~ '` + ipv4HostPattern + `' THEN r.host ELSE COALESCE(NULLIF(s.lan_ip, ''), r.host) END`

// querier is the read surface shared by *pgxpool.Pool and pgx.Tx, so the
// single-source desired-state query can run either standalone or inside the
// snapshot transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Row holds a resource record with joined shield + network names.
type Row struct {
	ID              string
	Name            string
	Description     *string
	Host            string
	Protocol        string
	PortFrom        int
	PortTo          int
	Status          string
	PendingAction   string // "apply" or "remove" — meaningful only when status="protecting"
	ErrorMessage    *string
	AppliedAt       *time.Time
	LastVerifiedAt  *time.Time
	CreatedAt       time.Time
	ShieldID        string
	ConnectorID     string // connector that manages this shield — needed for stream push
	ShieldName      *string
	ShieldStatus    *string
	RemoteNetworkID string
	NetworkName     string
	// ShieldHost is the host its shield protects (shieldHostSQL): Host for an
	// IPv4 resource, the bound shield's LAN IP for a DNS-name resource. Use it,
	// never Host, in instructions to a shield (PENDING-14 F-5).
	ShieldHost string
}

// PendingRow is a minimal resource record used in stream instruction delivery.
type PendingRow struct {
	ID            string
	Host          string
	Protocol      string
	PortFrom      int
	PortTo        int
	PendingAction string // "apply" or "remove"
}

// CreateInput holds fields provided by the admin when creating a resource.
type CreateInput struct {
	RemoteNetworkID string
	Name            string
	Description     *string
	Host            string
	Protocol        string
	PortFrom        int
	PortTo          int
}

const resourceSelectCols = `
	r.id, r.name, r.description, r.host, r.protocol, r.port_from, r.port_to,
	r.status, r.pending_action, r.error_message, r.applied_at, r.last_verified_at, r.created_at,
	COALESCE(r.shield_id::text, ''), r.remote_network_id,
	s.name, s.status, COALESCE(s.connector_id::text, ''),
	COALESCE(rn.name, ''),
	` + shieldHostSQL

// The network join requires the network to belong to the resource's own
// workspace: a legacy row pointing at another workspace's network still lists
// (so the admin can fix it) but never shows that network's name.
const resourceJoins = `
	FROM resources r
	LEFT JOIN shields s ON s.id = r.shield_id
	LEFT JOIN remote_networks rn ON rn.id = r.remote_network_id AND rn.tenant_id = r.tenant_id`

func scanRow(s interface{ Scan(...any) error }) (*Row, error) {
	var row Row
	if err := s.Scan(
		&row.ID, &row.Name, &row.Description, &row.Host, &row.Protocol,
		&row.PortFrom, &row.PortTo, &row.Status, &row.PendingAction, &row.ErrorMessage,
		&row.AppliedAt, &row.LastVerifiedAt, &row.CreatedAt,
		&row.ShieldID, &row.RemoteNetworkID,
		&row.ShieldName, &row.ShieldStatus, &row.ConnectorID,
		&row.NetworkName,
		&row.ShieldHost,
	); err != nil {
		return nil, err
	}
	return &row, nil
}

// AutoMatchShield finds an active shield whose LAN IP matches the resource host
// within its remote network. The same private IP may exist in multiple networks.
func AutoMatchShield(ctx context.Context, db *pgxpool.Pool, host, tenantID, remoteNetworkID string) (shieldID string, err error) {
	err = db.QueryRow(ctx,
		`SELECT id
		   FROM shields
		  WHERE lan_ip = $1
		    AND tenant_id = $2
		    AND remote_network_id = $3
		    AND status = 'active'
		  LIMIT 1`,
		host, tenantID, remoteNetworkID,
	).Scan(&shieldID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrNoMatchingShield, host)
		}
		return "", fmt.Errorf("auto-match shield: %w", err)
	}
	return shieldID, nil
}

// Create inserts an unprotected resource. A matching active Shield is retained
// as a convenience for a later Protect operation, but is not required (an IPv4
// host only: a DNS name has no LAN IP to match, PENDING-14 F-5). The remote
// network must belong to tenantID; the INSERT itself checks it, so nothing is
// written otherwise (ErrRemoteNetworkNotFound). A DNS-name host must use TCP
// (ErrNameNeedsTCP) and must not collide with the same name in another remote
// network (ErrNameCollision, checked under a lock in the same transaction).
func Create(ctx context.Context, db *pgxpool.Pool, tenantID string, input CreateInput) (*Row, error) {
	host, kind, err := NormalizeHost(input.Host)
	if err != nil {
		return nil, err
	}
	input.Host = host
	if kind == HostName && input.Protocol != "tcp" {
		return nil, ErrNameNeedsTCP
	}
	if _, err := uuid.Parse(input.RemoteNetworkID); err != nil {
		return nil, ErrRemoteNetworkNotFound
	}
	var shieldArg any
	if kind == HostIPv4 {
		shieldID, err := AutoMatchShield(ctx, db, input.Host, tenantID, input.RemoteNetworkID)
		if err != nil && !errors.Is(err, ErrNoMatchingShield) {
			return nil, err
		}
		if shieldID != "" {
			shieldArg = shieldID
		}
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}
	defer tx.Rollback(ctx)
	if kind == HostName {
		if err := checkNameCollision(ctx, tx, tenantID, host, input.RemoteNetworkID, "", input.PortFrom, input.PortTo); err != nil {
			return nil, err
		}
		if afterNameCollisionCheck != nil {
			afterNameCollisionCheck()
		}
	}

	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO resources
		    (tenant_id, remote_network_id, shield_id, name, description, protocol, host,
		     port_from, port_to, status, pending_action)
		 SELECT rn.tenant_id, rn.id, $3::uuid, $4::text, $5::text, $6::text, $7::text,
		        $8::int, $9::int, 'unprotected', 'remove'
		   FROM remote_networks rn
		  WHERE rn.id = $2 AND rn.tenant_id = $1
		 RETURNING id`,
		tenantID, input.RemoteNetworkID, shieldArg,
		input.Name, input.Description, input.Protocol, input.Host, input.PortFrom, input.PortTo,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRemoteNetworkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("create resource: commit: %w", err)
	}

	return GetByID(ctx, db, tenantID, id)
}

// afterNameCollisionCheck is a test seam: when set, Create calls it between
// the collision check and the INSERT, to widen the window two concurrent saves
// would race in without the lock. Always nil outside tests.
var afterNameCollisionCheck func()

// checkNameCollision refuses a DNS-name resource whose ports overlap a resource
// with the same name in another remote network of the workspace (PENDING-14
// F-6): a client allowed both could not tell which network to reach. It takes
// a transaction-scoped advisory lock on (workspace, name) first, so two
// concurrent saves of the same name serialise and the second sees the first.
// excludeID skips the resource being updated. IPv4 hosts are not checked: the
// same private IP legitimately exists in several networks.
func checkNameCollision(ctx context.Context, tx pgx.Tx, tenantID, host, networkID, excludeID string, portFrom, portTo int) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('resource-name:' || $1 || ':' || $2, 0))`,
		tenantID, host,
	); err != nil {
		return fmt.Errorf("check name collision: lock: %w", err)
	}
	var clash bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM resources
		      WHERE tenant_id = $1 AND host = $2
		        AND remote_network_id::text <> $3
		        AND id::text <> $4
		        AND port_from <= $6 AND port_to >= $5)`,
		tenantID, host, networkID, excludeID, portFrom, portTo,
	).Scan(&clash); err != nil {
		return fmt.Errorf("check name collision: %w", err)
	}
	if clash {
		return ErrNameCollision
	}
	return nil
}

// GetByID fetches a single resource by id + tenant.
func GetByID(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (*Row, error) {
	row, err := scanRow(db.QueryRow(ctx,
		`SELECT `+resourceSelectCols+resourceJoins+`
		  WHERE r.id = $1 AND r.tenant_id = $2`,
		id, tenantID,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resource not found")
		}
		return nil, fmt.Errorf("get resource: %w", err)
	}
	return row, nil
}

// GetByRemoteNetwork returns all resources for a remote network.
func GetByRemoteNetwork(ctx context.Context, db *pgxpool.Pool, tenantID, remoteNetworkID string) ([]*Row, error) {
	rows, err := db.Query(ctx,
		`SELECT `+resourceSelectCols+resourceJoins+`
		  WHERE r.remote_network_id = $1 AND r.tenant_id = $2
		  ORDER BY r.created_at DESC`,
		remoteNetworkID, tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	defer rows.Close()
	return collectRows(rows)
}

// GetAll returns all resources for a tenant.
func GetAll(ctx context.Context, db *pgxpool.Pool, tenantID string) ([]*Row, error) {
	rows, err := db.Query(ctx,
		`SELECT `+resourceSelectCols+resourceJoins+`
		  WHERE r.tenant_id = $1
		  ORDER BY r.created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list all resources: %w", err)
	}
	defer rows.Close()
	return collectRows(rows)
}

// GetByIDs fetches a tenant-scoped set of resources in one query. Results are
// returned in input order; a missing or cross-tenant ID fails the whole read.
func GetByIDs(
	ctx context.Context,
	db *pgxpool.Pool,
	tenantID string,
	ids []uuid.UUID,
) ([]*Row, error) {
	if len(ids) == 0 {
		return []*Row{}, nil
	}

	rows, err := db.Query(ctx,
		`SELECT `+resourceSelectCols+resourceJoins+`
		  WHERE r.tenant_id = $1
		    AND r.id = ANY($2)`,
		tenantID,
		ids,
	)
	if err != nil {
		return nil, fmt.Errorf("get resources by IDs: %w", err)
	}
	defer rows.Close()

	loaded, err := collectRows(rows)
	if err != nil {
		return nil, fmt.Errorf("collect resources by IDs: %w", err)
	}
	byID := make(map[string]*Row, len(loaded))
	for _, row := range loaded {
		byID[row.ID] = row
	}

	ordered := make([]*Row, 0, len(ids))
	for _, id := range ids {
		row, ok := byID[id.String()]
		if !ok {
			return nil, fmt.Errorf("resource %s not found", id)
		}
		ordered = append(ordered, row)
	}
	return ordered, nil
}

// GetPendingForShield returns resources in the protecting and deleting state for a shield.
// Called on stream connect to deliver any queued instructions to a reconnecting connector.
func GetPendingForShield(ctx context.Context, db *pgxpool.Pool, shieldID string) ([]*PendingRow, error) {
	rows, err := db.Query(ctx,
		`SELECT r.id, `+shieldHostSQL+`, r.protocol, r.port_from, r.port_to, r.pending_action
		   FROM resources r
		   LEFT JOIN shields s ON s.id = r.shield_id
		  WHERE r.shield_id = $1
		    AND r.status IN ('protecting', 'deleting')`,
		shieldID,
	)
	if err != nil {
		return nil, fmt.Errorf("get pending resources: %w", err)
	}
	defer rows.Close()

	var result []*PendingRow
	for rows.Next() {
		var r PendingRow
		if err := rows.Scan(&r.ID, &r.Host, &r.Protocol, &r.PortFrom, &r.PortTo, &r.PendingAction); err != nil {
			return nil, err
		}
		result = append(result, &r)
	}
	return result, rows.Err()
}

// desiredForShield is the SINGLE definition of a shield's desired set — the
// complete set of resources that should be enforced on it right now. Everything
// that needs "what should this shield be enforcing" (snapshot build, reconciler
// drift detection) routes through here, so the predicate cannot drift between
// callers or into SQL elsewhere.
//
// Includes (fail-closed):
//   - protected
//   - failed — the admin's intent is "protected"; a failed resource (e.g. the
//     'port not listening' case, where the shield HAS applied the drop rule) must
//     keep its rule so a temporarily-down service stays protected and is enforced
//     the instant it returns. Re-applying a host-mismatch/nftables-error 'failed'
//     is a harmless no-op on the shield.
//   - protecting + apply (in-flight protect)
//
// 'deleting', 'unprotected', 'pending', and 'protecting/remove' are intentionally
// absent — the shield's replace-semantics drops anything not listed here.
func desiredForShield(ctx context.Context, q querier, shieldID string) ([]*PendingRow, error) {
	rows, err := q.Query(ctx,
		`SELECT r.id, `+shieldHostSQL+`, r.protocol, r.port_from, r.port_to, r.pending_action
		   FROM resources r
		   LEFT JOIN shields s ON s.id = r.shield_id
		  WHERE r.shield_id = $1
		    AND (r.status IN ('protected', 'failed')
		         OR (r.status = 'protecting' AND r.pending_action = 'apply'))`,
		shieldID,
	)
	if err != nil {
		return nil, fmt.Errorf("get desired resources: %w", err)
	}
	defer rows.Close()

	var result []*PendingRow
	for rows.Next() {
		var r PendingRow
		if err := rows.Scan(&r.ID, &r.Host, &r.Protocol, &r.PortFrom, &r.PortTo, &r.PendingAction); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		result = append(result, &r)
	}
	return result, rows.Err()
}

// GetDesiredForShield returns the shield's desired set for read-only callers
// (the reconciler's drift detection). Snapshot delivery uses BuildShieldSnapshot
// instead, which pairs the same set with a generation.
func GetDesiredForShield(ctx context.Context, db *pgxpool.Pool, shieldID string) ([]*PendingRow, error) {
	return desiredForShield(ctx, db, shieldID)
}

// SnapshotResult is a shield's desired set paired with the monotonic generation
// that stamps it.
type SnapshotResult struct {
	Generation uint64
	Resources  []*PendingRow
}

// BuildShieldSnapshot returns the shield's desired set with a generation suitable
// for the shield's `generation <= last` staleness gate (ADR-004 Phase 2 / F11).
//
// Generation is opaque bookkeeping, never a desired-state rule: we hash the exact
// rows desiredForShield returns and bump the stored generation only when that
// fingerprint changes. Everything runs in one transaction with a row lock on the
// shield, so concurrent builders serialize and the (generation, fingerprint,
// content) triple stays consistent — a later-committed desired state always yields
// a higher generation, which is what lets the shield resolve out-of-order
// deliveries. Identical content reuses the generation, so the shield dedups it and
// metadata/audit writes never churn the value.
func BuildShieldSnapshot(ctx context.Context, db *pgxpool.Pool, shieldID string) (*SnapshotResult, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		gen      int64
		storedFP string
	)
	// Lock the shield row: without it two concurrent builders could read the same
	// generation and assign it to different content.
	if err := tx.QueryRow(ctx,
		`SELECT snapshot_generation, snapshot_fingerprint FROM shields WHERE id = $1 FOR UPDATE`,
		shieldID,
	).Scan(&gen, &storedFP); err != nil {
		return nil, fmt.Errorf("lock shield for snapshot: %w", err)
	}

	desired, err := desiredForShield(ctx, tx, shieldID)
	if err != nil {
		return nil, err
	}

	if fp := fingerprintDesired(desired); fp != storedFP {
		gen++
		if _, err := tx.Exec(ctx,
			`UPDATE shields SET snapshot_generation = $2, snapshot_fingerprint = $3 WHERE id = $1`,
			shieldID, gen, fp,
		); err != nil {
			return nil, fmt.Errorf("bump snapshot generation: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit snapshot tx: %w", err)
	}
	return &SnapshotResult{Generation: uint64(gen), Resources: desired}, nil
}

// fingerprintDesired is a stable content hash of a desired set. Sorted by resource
// ID so a reordered query result is not mistaken for a change.
//
// It hashes EXACTLY the fields the snapshot's ResourceInstruction carries on the
// wire — id, host, protocol, ports. pending_action is deliberately excluded: a
// snapshot has no per-row action (every listed row means "enforce this"), so
// hashing it would couple the generation to controller bookkeeping and churn the
// version without any change the shield can observe. A row entering or leaving the
// desired set is already reflected by its id appearing/disappearing here.
func fingerprintDesired(rows []*PendingRow) string {
	sorted := make([]*PendingRow, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	h := sha256.New()
	for _, r := range sorted {
		fmt.Fprintf(h, "%s|%s|%s|%d|%d\n", r.ID, r.Host, r.Protocol, r.PortFrom, r.PortTo)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// UpdateInput holds the fields that can be changed on an existing resource.
// Only non-nil fields are written to the database.
type UpdateInput struct {
	RemoteNetworkID *string
	Name            *string
	Description     *string
	Host            *string
	Protocol        *string
	PortFrom        *int
	PortTo          *int
}

// ShieldPush names a shield whose desired-state snapshot must be re-pushed after
// an update (connector.PushSnapshotForShield).
type ShieldPush struct {
	ConnectorID string
	ShieldID    string
}

// UpdateResult is an applied update plus the shield work it requires.
type UpdateResult struct {
	Row *Row
	// Moved: a protected resource's host or network now matches a different
	// shield. It is bound to that shield as protecting/apply, so the caller also
	// delivers the apply instruction (as ProtectResource does).
	Moved bool
	// ShieldPushes lists the shields whose snapshot changed, the new shield first
	// so the new host is firewalled before the old shield drops its rule.
	ShieldPushes []ShieldPush
}

// ErrResourceBusy: a shield-relevant edit while the resource is being
// unprotected (protecting/remove) or deleted.
var ErrResourceBusy = errors.New("resource is being unprotected or deleted; wait for it to finish")

// Update modifies editable fields on a resource. Only non-nil fields are applied.
//
// Sprint 20 Fix 18 (F24): a shield-enforced resource (protected, failed, or
// protecting/apply)
//   - goes back to protecting/apply and re-pushes its shield when host,
//     protocol, ports or network actually change
//     (values compared, as the admin UI sends every field), so the shield
//     firewalls the new port at once (before, it kept the old one and the new
//     port stayed open on the LAN);
//   - moves to the active shield on the new host when host or remote network
//     change (a shield protects only its own LAN IP), or is
//     refused with ErrNoMatchingShield if there is none. The old shield only gets
//     a snapshot without the resource, never a remove instruction, so it sends no
//     'unprotected' ack that could undo the move.
func Update(ctx context.Context, db *pgxpool.Pool, tenantID, id string, input UpdateInput) (*UpdateResult, error) {
	if input.Host != nil {
		h, _, err := NormalizeHost(*input.Host)
		if err != nil {
			return nil, err
		}
		input.Host = &h
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("update resource: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		status, action, host, networkID, protocol string
		portFrom, portTo                          int
		shieldID, connectorID                     *string
	)
	err = tx.QueryRow(ctx,
		`SELECT r.status, r.pending_action, r.host, r.remote_network_id::text, r.protocol, r.port_from, r.port_to,
		        r.shield_id::text, s.connector_id::text
		   FROM resources r
		   LEFT JOIN shields s ON s.id = r.shield_id
		  WHERE r.id = $1 AND r.tenant_id = $2
		    FOR UPDATE OF r`,
		id, tenantID,
	).Scan(&status, &action, &host, &networkID, &protocol, &portFrom, &portTo, &shieldID, &connectorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("resource not found")
	}
	if err != nil {
		return nil, fmt.Errorf("update resource: %w", err)
	}
	// Enforced: the shield holds (or is applying) the resource's rule. An edit
	// while a protect is still in flight (e.g. its shield is offline) is allowed
	// and simply replaces the pending rule.
	enforced := status == "protected" || status == "failed" || (status == "protecting" && action == "apply")

	// The resulting values, for the DNS-name rules (PENDING-14 F-1, F-6).
	finalHost, finalNetworkID, finalProtocol, finalFrom, finalTo := host, networkID, protocol, portFrom, portTo
	if input.Host != nil {
		finalHost = *input.Host
	}
	if input.RemoteNetworkID != nil {
		finalNetworkID = *input.RemoteNetworkID
	}
	if input.Protocol != nil {
		finalProtocol = *input.Protocol
	}
	if input.PortFrom != nil {
		finalFrom = *input.PortFrom
	}
	if input.PortTo != nil {
		finalTo = *input.PortTo
	}
	finalIsName := IsNameHost(finalHost)
	if finalIsName && finalProtocol != "tcp" {
		return nil, ErrNameNeedsTCP
	}
	if finalIsName && enforced && finalNetworkID != networkID {
		// A protected name is bound to a shield chosen by the admin; there is
		// no LAN IP to find a shield in the new network by.
		return nil, ErrNameProtectedMove
	}
	if finalIsName {
		if err := checkNameCollision(ctx, tx, tenantID, finalHost, finalNetworkID, id, finalFrom, finalTo); err != nil {
			return nil, err
		}
	}

	// Compare values, not presence: the admin UI sends every field on each save.
	// A new DNS-name host doesn't change what the shield enforces: the shield
	// protects its own LAN IP for a name (PENDING-14 F-5), and an IPv4 resource
	// was bound to the shield on that IP. So only a new IPv4 host counts.
	hostChangedForShield := changedStr(input.Host, host, true) && !finalIsName
	shieldChanged := hostChangedForShield || changedStr(input.RemoteNetworkID, networkID, false) ||
		changedStr(input.Protocol, protocol, false) || changedInt(input.PortFrom, portFrom) || changedInt(input.PortTo, portTo)
	if shieldChanged && ((status == "protecting" && action == "remove") || status == "deleting") {
		return nil, fmt.Errorf("update resource: %w", ErrResourceBusy)
	}

	args := []any{id, tenantID}
	sets := []string{"updated_at = NOW()"}

	add := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}

	newHost, newNetworkID := host, networkID
	if input.RemoteNetworkID != nil {
		// A new remote network must belong to tenantID; a foreign or missing
		// one changes nothing. Checked inside this transaction, and a network
		// never moves to another tenant, so the answer can't go stale before
		// the UPDATE below.
		if _, err := uuid.Parse(*input.RemoteNetworkID); err != nil {
			return nil, ErrRemoteNetworkNotFound
		}
		var owned bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM remote_networks WHERE id = $1 AND tenant_id = $2)`,
			*input.RemoteNetworkID, tenantID,
		).Scan(&owned); err != nil {
			return nil, fmt.Errorf("update resource: check remote network: %w", err)
		}
		if !owned {
			return nil, ErrRemoteNetworkNotFound
		}
		add("remote_network_id", *input.RemoteNetworkID)
		newNetworkID = *input.RemoteNetworkID
	}
	if input.Name != nil {
		add("name", *input.Name)
	}
	if input.Description != nil {
		add("description", *input.Description)
	}
	if input.Host != nil {
		newHost = strings.TrimSpace(*input.Host)
		add("host", newHost)
	}
	if input.Protocol != nil {
		add("protocol", *input.Protocol)
	}
	if input.PortFrom != nil {
		add("port_from", *input.PortFrom)
	}
	if input.PortTo != nil {
		add("port_to", *input.PortTo)
	}

	result := &UpdateResult{}
	if enforced && !finalIsName && (newHost != host || newNetworkID != networkID) {
		var newShieldID, newConnectorID string
		err := tx.QueryRow(ctx,
			`SELECT s.id::text, s.connector_id::text
			   FROM shields s
			  WHERE s.tenant_id = $1 AND s.remote_network_id::text = $2
			    AND s.lan_ip = $3 AND s.status = 'active'
			  ORDER BY s.created_at
			  LIMIT 1`,
			tenantID, newNetworkID, newHost,
		).Scan(&newShieldID, &newConnectorID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("update resource: no active (online) shield on %s in the resource's remote network; a protected resource's host must be a shield's LAN IP (unprotect it first, or bring that shield online): %w", newHost, ErrNoMatchingShield)
		}
		if err != nil {
			return nil, fmt.Errorf("update resource: match shield: %w", err)
		}
		if shieldID == nil || *shieldID != newShieldID {
			add("shield_id", newShieldID)
			result.Moved = true
			result.ShieldPushes = append(result.ShieldPushes, ShieldPush{ConnectorID: newConnectorID, ShieldID: newShieldID})
		}
	}
	if enforced && shieldChanged {
		// Re-protect with the new settings: the shield swaps the old rule for the
		// new one in one nftables transaction (no unprotected gap) and acks, which
		// turns protecting back into protected (or failed). An offline shield gets
		// the rule from the connector's cached snapshot when it reconnects; until
		// then the resource shows protecting and the NEW port is not firewalled
		// (the old rule stays). A move needs an active target shield, so a host
		// change never leaves the new host unprotected.
		sets = append(sets, "status = 'protecting'", "pending_action = 'apply'", "error_message = NULL")
		if shieldID != nil && connectorID != nil {
			result.ShieldPushes = append(result.ShieldPushes, ShieldPush{ConnectorID: *connectorID, ShieldID: *shieldID})
		}
	}

	query := fmt.Sprintf(
		`UPDATE resources SET %s
		  WHERE id = $1 AND tenant_id = $2`,
		joinSets(sets),
	)
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		return nil, fmt.Errorf("update resource: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("update resource: commit: %w", err)
	}
	row, err := GetByID(ctx, db, tenantID, id)
	if err != nil {
		return nil, err
	}
	result.Row = row
	return result, nil
}

// changedStr reports whether an update field differs from the stored value
// (Sprint 20 Fix 18): a nil field, or an equal value, is no change.
func changedStr(in *string, cur string, trim bool) bool {
	if in == nil {
		return false
	}
	v := *in
	if trim {
		v = strings.TrimSpace(v)
	}
	return v != cur
}

func changedInt(in *int, cur int) bool {
	return in != nil && *in != cur
}

// ACLRelevantUpdate reports whether an UpdateInput touches a field the ACL
// compiler actually reads (policy.ListEnabledRulesWithResources selects name,
// host, port_from, protocol, shield, status and remote_network_id; the compiler
// routes each resource through its remote network's connectors). Of those,
// UpdateInput can change name, host (Fix 18), protocol, port_from and
// remote_network_id.
// description and port_to are invisible to the compiler, so changing only those
// must not invalidate the per-workspace ACL snapshot. Callers use this to decide
// whether to fire NotifyPolicyChange after an UpdateResource.
//
// Sprint 20 Fix 02: remote_network_id was missing (the comment called it
// invisible); an unconditional notify in UpdateResource hid that until Fix 02
// removed the duplicate.
func ACLRelevantUpdate(input UpdateInput) bool {
	return input.Name != nil || input.Host != nil || input.Protocol != nil || input.PortFrom != nil ||
		input.RemoteNetworkID != nil
}

func joinSets(sets []string) string {
	var b strings.Builder
	for i, s := range sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(s)
	}
	return b.String()
}

// ErrProtectNoShield answers a protect that found no usable shield: the
// resource doesn't exist in this workspace, is in a state that can't be
// protected, or no matching active shield is in its remote network. One answer
// for all, so a foreign resource or shield looks exactly like a missing one.
var ErrProtectNoShield = apperr.UserErrorf("protect resource: resource not found, invalid state, or no matching active shield in its remote network")

// ErrProtectNameNeedsShield asks the admin to choose the shield for a DNS-name
// resource: the controller can't resolve the name, so it can't match a shield
// by LAN IP (PENDING-14 F-5).
var ErrProtectNameNeedsShield = apperr.UserErrorf("protect resource: a resource with a DNS name needs a shield chosen to protect it")

// MarkProtecting atomically matches an active Shield and transitions the
// resource to protected intent. ACL routing becomes Shield-based immediately.
//
// IPv4 host: the shield is the active one in the resource's remote network
// whose LAN IP is the host, preferring the one already bound. shieldID may be
// empty; if given, it must be that kind of shield.
//
// DNS-name host (PENDING-14 F-5): shieldID is required and must be an active
// shield in the resource's workspace and remote network. The name is an alias
// for that shield's host; the shield is told its own LAN IP (shieldHostSQL).
func MarkProtecting(ctx context.Context, db *pgxpool.Pool, tenantID, id, shieldID string) (*Row, error) {
	if shieldID != "" {
		if _, err := uuid.Parse(shieldID); err != nil {
			return nil, ErrProtectNoShield
		}
	}
	var discardedID string
	err := db.QueryRow(ctx,
		`WITH candidate AS (
		     SELECT s.id
		       FROM resources r
		       JOIN shields s
		         ON s.tenant_id = r.tenant_id
		        AND s.remote_network_id = r.remote_network_id
		        AND s.status = 'active'
		      WHERE r.id = $1 AND r.tenant_id = $2
		        AND r.status = ANY($3)
		        AND (
		              (r.host ~ '`+ipv4HostPattern+`' AND s.lan_ip = r.host AND ($4 = '' OR s.id::text = $4))
		           OR (r.host !~ '`+ipv4HostPattern+`' AND s.id::text = $4)
		            )
		      ORDER BY CASE WHEN s.id = r.shield_id THEN 0 ELSE 1 END
		      LIMIT 1
		 )
		 UPDATE resources r
		    SET shield_id = candidate.id,
		        status = 'protecting', pending_action = 'apply',
		        error_message = NULL, updated_at = NOW()
		   FROM candidate
		  WHERE r.id = $1 AND r.tenant_id = $2
		 RETURNING r.id`,
		id, tenantID, []string{"pending", "failed", "unprotected"}, shieldID,
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if shieldID == "" {
				var host string
				if db.QueryRow(ctx, `SELECT host FROM resources WHERE id = $1 AND tenant_id = $2`, id, tenantID).Scan(&host) == nil && IsNameHost(host) {
					return nil, ErrProtectNameNeedsShield
				}
			}
			return nil, ErrProtectNoShield
		}
		return nil, fmt.Errorf("protect resource: %w", err)
	}
	return GetByID(ctx, db, tenantID, id)
}

// MarkUnprotecting removes protection from a resource.
//
// Normally it transitions to 'protecting'/pending_action='remove' and waits for
// the shield to ack the nftables removal. But if the bound shield is REVOKED or
// gone (no shields row), there is no shield left to apply the removal or ack it,
// and the firewall rule died with the shield — so parking in 'protecting' would
// hang forever waiting for an ack that can never arrive (Finding 7). In that case
// we resolve straight to the terminal 'unprotected' state in the same atomic
// UPDATE (deciding off the shield status, so there is no TOCTOU). A merely
// disconnected/offline-but-not-revoked shield still takes the normal path: it
// picks up the pending remove and acks it on reconnect.
//
// pending_action stays 'remove' in both branches — it is only meaningful while
// status='protecting', and the normal terminal path (RecordAck) also leaves it at
// 'remove' when it flips to 'unprotected', so the resolve-through end-state is
// identical: ('unprotected', 'remove').
func MarkUnprotecting(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (*Row, error) {
	var discardedID string
	err := db.QueryRow(ctx,
		`UPDATE resources
		    SET status = CASE
		            WHEN COALESCE(
		                     (SELECT s.status FROM shields s WHERE s.id = resources.shield_id),
		                     'revoked'
		                 ) = 'revoked'
		            THEN 'unprotected'   -- shield revoked or gone: nothing to ack (Finding 7)
		            ELSE 'protecting'    -- live/recoverable shield: normal remove + ack path
		        END,
		        pending_action = 'remove',
		        updated_at = NOW()
		  WHERE id = $1 AND tenant_id = $2
		    AND status = 'protected'
		 RETURNING id`,
		id, tenantID,
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("unprotect resource: resource not found or not in protected state")
		}
		return nil, fmt.Errorf("unprotect resource: %w", err)
	}
	return GetByID(ctx, db, tenantID, id)
}

// MarkDeleting transitions a resource to the 'deleting' tombstone with
// pending_action='remove'. The row is NOT removed here — it is reaped by
// RecordAck once the shield confirms the nftables rule is gone. Only states
// that may hold a rule (protected, failed) take this path.
func MarkDeleting(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (*Row, error) {
	var discardedID string
	err := db.QueryRow(ctx,
		`UPDATE resources
                  SET status = 'deleting', pending_action = 'remove', updated_at = NOW()
                WHERE id = $1 AND tenant_id = $2
                  AND status IN ('protected', 'failed')
               RETURNING id`,
		id, tenantID,
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("delete resource: resource not found or in invalid state")
		}
		return nil, fmt.Errorf("mark deleting: %w", err)
	}
	return GetByID(ctx, db, tenantID, id)
}

// DeleteRow hard-deletes a resource that cannot have a shield rule (pending or
// unprotected). Protected/failed resources must go through MarkDeleting first.
func DeleteRow(ctx context.Context, db *pgxpool.Pool, tenantID, id string) error {
	var discardedID string
	err := db.QueryRow(ctx,
		`DELETE FROM resources
                WHERE id = $1 AND tenant_id = $2
                  AND status IN ('pending', 'unprotected')
               RETURNING id`,
		id, tenantID,
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("resource not found or must be unprotected before deleting")
		}
		return fmt.Errorf("delete resource: %w", err)
	}
	return nil
}

// DeleteIfShieldGone hard-deletes a resource that may hold a shield rule
// (protected, failed, or already tombstoned 'deleting') when its bound shield is
// revoked or gone (Sprint 20 Fix 16, F50). No shield can remove the rule or ack
// the removal, so waiting for one would leave the row in 'deleting' forever (the
// delete twin of MarkUnprotecting's Finding 7). Returns true if a row was deleted.
func DeleteIfShieldGone(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (bool, error) {
	ct, err := db.Exec(ctx,
		`DELETE FROM resources
		  WHERE id = $1 AND tenant_id = $2
		    AND status IN ('protected', 'failed', 'deleting')
		    AND COALESCE(
		            (SELECT s.status FROM shields s WHERE s.id = resources.shield_id),
		            'revoked'
		        ) = 'revoked'`,
		id, tenantID,
	)
	if err != nil {
		return false, fmt.Errorf("delete resource (shield gone): %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

// ReapedTombstone is a 'deleting' resource removed by ReapStaleTombstones.
type ReapedTombstone struct {
	ID          string
	TenantID    string
	ShieldID    string
	ConnectorID string
}

// ReapStaleTombstones hard-deletes every 'deleting' tombstone that has waited
// longer than olderThan for its shield's removal ack (Sprint 20 Fix 16, F50).
// A live shield acks a removal within milliseconds (the ack is forwarded at once,
// not batched); one that has not acked is offline, revoked or gone, and the
// controller only learns a shield is offline from batched status up to minutes
// later. Removing the row is safe: the shield's rule set follows the controller's
// desired-state snapshot, so the caller re-pushes the snapshot (a connected shield
// drops the rule now, an offline one on reconnect) and notifies the policy.
func ReapStaleTombstones(ctx context.Context, db *pgxpool.Pool, olderThan time.Duration) ([]ReapedTombstone, error) {
	rows, err := db.Query(ctx,
		`DELETE FROM resources r
		  WHERE r.status = 'deleting'
		    AND r.updated_at < NOW() - make_interval(secs => $1)
		 RETURNING r.id, r.tenant_id, COALESCE(r.shield_id::text, ''),
		           COALESCE((SELECT s.connector_id::text FROM shields s WHERE s.id = r.shield_id), '')`,
		olderThan.Seconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("reap stale tombstones: %w", err)
	}
	defer rows.Close()
	var reaped []ReapedTombstone
	for rows.Next() {
		var t ReapedTombstone
		if err := rows.Scan(&t.ID, &t.TenantID, &t.ShieldID, &t.ConnectorID); err != nil {
			return nil, fmt.Errorf("reap stale tombstones: %w", err)
		}
		reaped = append(reaped, t)
	}
	return reaped, rows.Err()
}

// RunTombstoneSweeper reaps stale tombstones every interval until ctx ends,
// calling onReaped for each removed resource (snapshot re-push + policy notify).
func RunTombstoneSweeper(ctx context.Context, db *pgxpool.Pool, interval, grace time.Duration, onReaped func(ReapedTombstone)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		reaped, err := ReapStaleTombstones(ctx, db, grace)
		if err != nil {
			log.Printf("resource tombstone sweeper: %v", err)
			continue
		}
		for _, t := range reaped {
			shieldID := t.ShieldID
			if shieldID == "" {
				shieldID = "(gone)"
			}
			log.Printf("resource tombstone sweeper: removed resource %s (no removal ack within %s from shield %s)", t.ID, grace, shieldID)
			onReaped(t)
		}
	}
}

// ForceDeleteRow hard-deletes a resource in ANY state, tenant-scoped. This is the
// break-glass escape hatch (ADR-004 Phase 4): it bypasses the confirmation-gated
// tombstone path (MarkDeleting → ack-driven reap) for a resource permanently stuck
// because its shield is gone and will never ack removal. Because it removes the
// record of intent WITHOUT observing the rule's removal, callers must be admin-only
// and MUST audit-log the action, then best-effort re-push the shield snapshot so a
// still-connected shield drops any lingering rule. Returns true if a row was deleted.
func ForceDeleteRow(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (bool, error) {
	ct, err := db.Exec(ctx,
		`DELETE FROM resources WHERE id = $1 AND tenant_id = $2`,
		id, tenantID,
	)
	if err != nil {
		return false, fmt.Errorf("force delete resource: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

// RecordAck processes a ResourceAck from Shield and updates the resource status.
// The ack's port_reachable is intentionally not a parameter: it carries no
// information beyond `status` (protected ⟺ reachable; "port not listening" ⟺ not),
// and there is no port_reachable column to persist it to. verified_at IS persisted
// as last_verified_at.
// RecordAck records a shield's ack of a resource instruction. It returns
// aclChanged=true when the ack changed the ACL compiler's output, so the caller
// must fire NotifyPolicyChange:
//   - an 'unprotected' ack physically removed a 'deleting' tombstone (the DELETE
//     below): the resource left the output;
//   - the status moved between the connector-routed states (pending,
//     unprotected) and the shield-routed ones (protecting, protected, failed):
//     the compiler routes by status (policy.routeTypeForResource), so e.g. an
//     unprotect completing ('protecting' → 'unprotected') re-routes the resource
//     from its shield to the connector (Sprint 20 Fix 02, F35: this transition
//     notified nothing, leaving the ACL snapshot stale).
//
// Updates within one route class (e.g. protecting → protected/failed) change no
// compiler-visible data and return false.
// Ack is a shield's report about one resource (shield.v1.ResourceAck as
// forwarded by the connector). ShieldID is the connector-stamped, verified
// shield identity; Host/Protocol/PortFrom/PortTo echo the rule the shield
// enforces (Sprint 20 Fix 18).
type Ack struct {
	ResourceID string
	Status     string
	Error      string
	VerifiedAt int64
	ShieldID   string
	Host       string
	Protocol   string
	PortFrom   int
	PortTo     int
}

func RecordAck(ctx context.Context, db *pgxpool.Pool, tenantID string, ack Ack) (aclChanged bool, err error) {
	if ack.Status == "unprotected" {
		ct, err := db.Exec(ctx,
			`DELETE FROM resources
			 WHERE id = $1 AND tenant_id = $2
			   AND status = 'deleting'
			   AND shield_id::text = $3`,
			ack.ResourceID, tenantID, ack.ShieldID,
		)
		if err != nil {
			return false, fmt.Errorf("record ack (reap): %w", err)
		}
		if ct.RowsAffected() > 0 {
			return true, nil
		}
	}
	var oldStatus string
	err = db.QueryRow(ctx,
		`UPDATE resources r
		    SET status           = $2,
		        error_message    = NULLIF($3, ''),
		        last_verified_at = to_timestamp($4),
		        applied_at       = CASE WHEN $2 = 'protected' AND r.applied_at IS NULL THEN NOW() ELSE r.applied_at END,
		        updated_at       = NOW()
		   FROM (SELECT id, status FROM resources WHERE id = $1 AND tenant_id = $5 FOR UPDATE) prev
		   LEFT JOIN shields s ON s.id::text = $6
		  WHERE r.id = prev.id
			AND r.status != 'deleting'
		    AND NOT (r.status = 'protecting' AND r.pending_action = 'remove' AND $2 != 'unprotected')
		    -- Fix 18: 'unprotected' completes only an unprotect (protecting/remove).
		    -- A stray one (e.g. a late ack from the shield a resource was moved
		    -- away from) must not flip a protected resource to connector routing.
		    AND ($2 != 'unprotected' OR (r.status = 'protecting' AND r.pending_action = 'remove'))
		    -- Fix 18: only the resource's current shield may ack it, and a
		    -- protected/failed verdict counts only for the current rule; an ack
		    -- the shield sent for the old host/port right before an edit is stale.
		    AND r.shield_id::text = $6
		    AND ($2 NOT IN ('protected', 'failed')
		         OR ((`+shieldHostSQL+`) = $7 AND r.protocol = $8 AND r.port_from = $9 AND r.port_to = $10))
		 RETURNING prev.status`,
		ack.ResourceID, ack.Status, ack.Error, ack.VerifiedAt, tenantID,
		ack.ShieldID, ack.Host, ack.Protocol, ack.PortFrom, ack.PortTo,
	).Scan(&oldStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		logIgnoredAck(ctx, db, tenantID, ack)
		return false, nil // guarded out: nothing changed
	}
	if err != nil {
		return false, fmt.Errorf("record ack: %w", err)
	}
	return connectorRouted(oldStatus) != connectorRouted(ack.Status), nil
}

// logIgnoredAck says why an ack changed nothing, when the reason is a wrong
// shield or a stale rule (Fix 18), so a resource stuck in 'protecting' can be
// diagnosed from the controller log. Status-guard cases stay silent: they are
// routine (a periodic ack while an unprotect is in flight).
func logIgnoredAck(ctx context.Context, db *pgxpool.Pool, tenantID string, ack Ack) {
	var (
		shieldID, host, protocol string
		portFrom, portTo         int
	)
	if err := db.QueryRow(ctx,
		`SELECT COALESCE(r.shield_id::text, ''), `+shieldHostSQL+`, r.protocol, r.port_from, r.port_to
		   FROM resources r
		   LEFT JOIN shields s ON s.id = r.shield_id
		  WHERE r.id = $1 AND r.tenant_id = $2`,
		ack.ResourceID, tenantID,
	).Scan(&shieldID, &host, &protocol, &portFrom, &portTo); err != nil {
		return
	}
	switch {
	case shieldID != ack.ShieldID:
		log.Printf("resource ack ignored: resource=%s status=%s from shield %q, but the resource is bound to %q",
			ack.ResourceID, ack.Status, ack.ShieldID, shieldID)
	case (ack.Status == "protected" || ack.Status == "failed") &&
		(host != ack.Host || protocol != ack.Protocol || portFrom != ack.PortFrom || portTo != ack.PortTo):
		log.Printf("resource ack ignored: resource=%s status=%s is for rule %s %s/%d-%d, current rule is %s %s/%d-%d (stale)",
			ack.ResourceID, ack.Status, ack.Host, ack.Protocol, ack.PortFrom, ack.PortTo, host, protocol, portFrom, portTo)
	}
}

// connectorRouted mirrors policy.routeTypeForResource: pending and unprotected
// resources are routed through the connector, every other status through the
// resource's shield.
func connectorRouted(status string) bool {
	return status == "pending" || status == "unprotected"
}

func collectRows(rows pgx.Rows) ([]*Row, error) {
	var result []*Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		result = []*Row{}
	}
	return result, nil
}

// GetDeletingForShield returns the ids of tombstoned ('deleting') resources on
// a shield. The Phase 3 reconciler reaps these once the shield's state reports
// confirm the rule is gone (ADR-004: confirmation-gated deletion).
func GetDeletingForShield(ctx context.Context, db *pgxpool.Pool, shieldID string) ([]string, error) {
	rows, err := db.Query(ctx,
		`SELECT id FROM resources WHERE shield_id = $1 AND status = 'deleting'`,
		shieldID,
	)
	if err != nil {
		return nil, fmt.Errorf("get deleting resources: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReapTombstone hard-deletes a 'deleting' tombstone after the shield's reported
// state confirmed the rule is absent. Returns true if a row was reaped.
func ReapTombstone(ctx context.Context, db *pgxpool.Pool, tenantID, shieldID, resourceID string) (bool, error) {
	ct, err := db.Exec(ctx,
		`DELETE FROM resources
		  WHERE id = $1 AND tenant_id = $2 AND shield_id = $3 AND status = 'deleting'`,
		resourceID, tenantID, shieldID,
	)
	if err != nil {
		return false, fmt.Errorf("reap tombstone: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

// ShieldHostSQL exports shieldHostSQL for other packages' shield-facing
// queries (internal/connector's signed resource set). Same r/s aliases.
const ShieldHostSQL = shieldHostSQL
