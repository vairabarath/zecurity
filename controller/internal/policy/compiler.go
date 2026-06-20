package policy

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

const defaultRelayPort = "9093"

// CompileACLSnapshot builds a fresh ACLSnapshot for the given workspace by
// walking: enabled access_rules → groups → group members → client device SPIFFE IDs.
//
// Returns an error (and no snapshot) on any DB failure — callers must default-deny.
func CompileACLSnapshot(ctx context.Context, store *Store, notifier *Notifier, pool *pgxpool.Pool, workspaceID string) (*clientv1.ACLSnapshot, error) {
	rules, err := store.ListEnabledRulesWithResources(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("compile acl: list rules: %w", err)
	}

	// Aggregate SPIFFE IDs per resource, deduplicating across groups.
	type entryKey struct {
		resourceID string
		address    string
		port       uint32
		protocol   string
	}
	spiffeSet := make(map[entryKey]map[string]struct{})
	names := make(map[entryKey]string)
	shieldIDs := make(map[entryKey]string)

	for _, rule := range rules {
		key := entryKey{
			resourceID: rule.ResourceID,
			address:    rule.Address,
			port:       rule.Port,
			protocol:   rule.Protocol,
		}
		if _, ok := spiffeSet[key]; !ok {
			spiffeSet[key] = make(map[string]struct{})
			names[key] = rule.Name
			shieldIDs[key] = rule.ShieldID
		}

		spiffes, err := store.ListActiveDeviceSPIFFEsForGroup(ctx, workspaceID, rule.GroupID)
		if err != nil {
			return nil, fmt.Errorf("compile acl: spiffes for group %s: %w", rule.GroupID, err)
		}
		for _, s := range spiffes {
			spiffeSet[key][s] = struct{}{}
		}
	}

	entries := make([]*clientv1.ACLEntry, 0, len(spiffeSet))
	for key, set := range spiffeSet {
		ids := make([]string, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		entries = append(entries, &clientv1.ACLEntry{
			ResourceId:       key.resourceID,
			Name:             names[key],
			Address:          key.address,
			Port:             key.port,
			Protocol:         key.protocol,
			AllowedSpiffeIds: ids,
			RouteType:        "shield",
			ShieldId:         shieldIDs[key],
		})
	}

	// Use the notifier's monotonic version so downstream clients can detect
	// policy changes. After a controller restart the counter resets to 0 but
	// increments on the next policy mutation — that is acceptable.
	version := notifier.Version(workspaceID)

	// Look up the active connector. Returns lan_addr (for connector_tunnel_addr),
	// id and trust_domain (used to derive connector_spiffe). Single query.
	// lan_addr may be stored as "ip:port" (gRPC port 9091) — extract only the host;
	// connector QUIC always runs on port 9092.
	var connectorTunnelAddr, connectorID, connectorSPIFFE string
	var lanAddr, trustDomain string
	_ = pool.QueryRow(ctx,
		`SELECT COALESCE(lan_addr, ''), id::text, COALESCE(trust_domain, '')
		 FROM connectors
		 WHERE tenant_id = $1
		   AND status = 'active'
		 ORDER BY last_heartbeat_at DESC NULLS LAST LIMIT 1`,
		workspaceID,
	).Scan(&lanAddr, &connectorID, &trustDomain)
	if lanAddr != "" {
		host := lanAddr
		if h, _, err := net.SplitHostPort(lanAddr); err == nil {
			host = h
		}
		connectorTunnelAddr = host + ":9092"
	}
	if connectorID != "" && trustDomain != "" {
		connectorSPIFFE = appmeta.ConnectorSPIFFEID(trustDomain, connectorID)
	}

	// Sprint 10.3 — Source the relay endpoint from the live relays table.
	// Prefer public_addr (operator-set or auto-inferred when observed scope is
	// public). Fall back to observed_ip:9093 when only the observed peer
	// address is known. Skip rows that are loopback/link-local/unknown — those
	// addresses aren't reachable by clients. No row → all four relay fields
	// stay empty, which the client interprets as "direct-only, no relay path".
	var relayAddr, relaySPIFFEID string
	var (
		relayRowID   string
		publicAddr   *string
		observedIP   *string
		observedPort *int
		addressScope *string
	)
	_ = pool.QueryRow(ctx,
		`SELECT id::text, public_addr, observed_ip::text, observed_port, address_scope
		   FROM relays
		  WHERE status = 'active'
		    AND (
		          public_addr IS NOT NULL
		          OR (observed_ip IS NOT NULL
		              AND address_scope IN ('public', 'private'))
		        )
		  ORDER BY last_heartbeat_at DESC NULLS LAST
		  LIMIT 1`,
	).Scan(&relayRowID, &publicAddr, &observedIP, &observedPort, &addressScope)
	switch {
	case publicAddr != nil && *publicAddr != "":
		relayAddr = *publicAddr
	case observedIP != nil && *observedIP != "":
		port := defaultRelayPort
		if observedPort != nil && *observedPort > 0 {
			port = strconv.Itoa(*observedPort)
		}
		relayAddr = net.JoinHostPort(*observedIP, port)
	}
	if relayAddr != "" {
		relaySPIFFEID = appmeta.RelaySPIFFEID(relayRowID)
	}

	return &clientv1.ACLSnapshot{
		WorkspaceId:         workspaceID,
		Version:             version,
		GeneratedAt:         time.Now().Unix(),
		Entries:             entries,
		ConnectorTunnelAddr: connectorTunnelAddr,
		ConnectorId:         connectorID,
		ConnectorSpiffe:     connectorSPIFFE,
		RelayAddr:           relayAddr,
		RelaySpiffeId:       relaySPIFFEID,
	}, nil
}
