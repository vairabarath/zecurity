package transport

import (
	"context"
	"fmt"
	"net"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

const (
	// defaultRelayPort is the relay QUIC port used when only an observed IP is
	// known (no public_addr override). Matches policy.defaultRelayPort.
	defaultRelayPort = "9093"
	// connectorTunnelPort is the connector's inner-tunnel QUIC port.
	connectorTunnelPort = "9092"
)

// CompileTransportSnapshot builds the workspace's transport snapshot: every
// active connector grouped by remote_network_id, with its tunnel + relay
// coordinates. It is the transport-plane analogue of policy.CompileACLSnapshot's
// connector section — same derivation of tunnel addr, SPIFFE, and relay addr so
// both planes resolve connectivity identically. Returns an error on any DB
// failure; never returns a partial snapshot.
func CompileTransportSnapshot(ctx context.Context, store *Store, notifier *Notifier, workspaceID string) (*clientv1.TransportSnapshot, error) {
	rows, err := store.GetWorkspaceConnectors(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("compile transport: connector lookup: %w", err)
	}

	rnMap := make(map[string]*clientv1.TransportRemoteNetwork)
	order := make([]string, 0) // preserve query order for deterministic output
	for _, row := range rows {
		host := row.LanAddr
		if h, _, err := net.SplitHostPort(row.LanAddr); err == nil {
			host = h
		}
		tunnelAddr := ""
		if host != "" {
			// JoinHostPort brackets IPv6 correctly (e.g. "[2001:db8::1]:9092").
			tunnelAddr = net.JoinHostPort(host, connectorTunnelPort)
		}
		spiffe := ""
		if row.ConnectorID != "" && row.TrustDomain != "" {
			spiffe = appmeta.ConnectorSPIFFEID(row.TrustDomain, row.ConnectorID)
		}
		relaySpiffe := ""
		if row.RelayID != "" {
			relaySpiffe = appmeta.RelaySPIFFEID(row.RelayID)
		}

		rn, ok := rnMap[row.RemoteNetworkID]
		if !ok {
			rn = &clientv1.TransportRemoteNetwork{RemoteNetworkId: row.RemoteNetworkID}
			rnMap[row.RemoteNetworkID] = rn
			order = append(order, row.RemoteNetworkID)
		}
		rn.Connectors = append(rn.Connectors, &clientv1.TransportConnector{
			ConnectorId:         row.ConnectorID,
			ConnectorTunnelAddr: tunnelAddr,
			ConnectorSpiffe:     spiffe,
			RelayAddr:           resolveConnectorRelayAddr(row.RelayPublicAddr, row.RelayObservedHost),
			RelaySpiffeId:       relaySpiffe,
		})
	}

	remoteNetworks := make([]*clientv1.TransportRemoteNetwork, 0, len(order))
	for _, id := range order {
		remoteNetworks = append(remoteNetworks, rnMap[id])
	}
	return &clientv1.TransportSnapshot{
		RemoteNetworks: remoteNetworks,
		Version:        notifier.Version(workspaceID),
	}, nil
}

// resolveConnectorRelayAddr mirrors policy/compiler.go's helper (kept local so
// the transport plane stays decoupled from the policy package). A configured
// public_addr wins as-is; otherwise the observed IP of a public-scope relay is
// joined with the default relay port (net.JoinHostPort brackets IPv6). Returns
// "" when the connector has no relay coordinates.
func resolveConnectorRelayAddr(publicAddr, observedHost string) string {
	if publicAddr != "" {
		return publicAddr
	}
	if observedHost != "" {
		return net.JoinHostPort(observedHost, defaultRelayPort)
	}
	return ""
}

// Compiler bundles the store, cache, and notifier so callers (the client RPC
// handler and the connector control-stream push) can get a workspace's
// transport snapshot through one dependency, cached with epoch-CAS.
type Compiler struct {
	store    *Store
	cache    *SnapshotCache
	notifier *Notifier
}

// NewCompiler wires a Compiler over the transport store, cache, and notifier.
func NewCompiler(store *Store, cache *SnapshotCache, notifier *Notifier) *Compiler {
	return &Compiler{store: store, cache: cache, notifier: notifier}
}

// GetOrCompile returns the workspace's transport snapshot from cache, compiling
// (under epoch CAS) on a miss.
func (c *Compiler) GetOrCompile(ctx context.Context, workspaceID string) (*clientv1.TransportSnapshot, error) {
	return c.cache.GetOrCompile(workspaceID, func() (*clientv1.TransportSnapshot, error) {
		return CompileTransportSnapshot(ctx, c.store, c.notifier, workspaceID)
	})
}
