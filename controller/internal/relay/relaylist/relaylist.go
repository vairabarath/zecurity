// Package relaylist holds the set of per-pool relay lists sent to connectors
// (Decision Record D-38). It is separate from relaypool because it depends on
// the gRPC-generated connector types, which providerquery must not import
// (D-04).
package relaylist

import (
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/relay/relaypool"
)

// Set is the relay list of every pool plus each workspace's pool, built in one
// pass for a broadcast. ForWorkspace is what one connector receives.
type Set struct {
	lists map[string]*pb.LabelledRelayList // pool id → list (pools with eligible relays)
	pools map[string]string                // workspace id → pool id
	empty *pb.LabelledRelayList            // what a pool without eligible relays sends
}

// NewSet builds a Set. lists holds the pools that have eligible relays;
// workspacePools maps each workspace to its effective pool; empty is the
// (versioned) list a pool without eligible relays sends.
func NewSet(lists map[string]*pb.LabelledRelayList, workspacePools map[string]string, empty *pb.LabelledRelayList) *Set {
	if lists == nil {
		lists = map[string]*pb.LabelledRelayList{}
	}
	if workspacePools == nil {
		workspacePools = map[string]string{}
	}
	return &Set{lists: lists, pools: workspacePools, empty: empty}
}

// PoolOf returns a workspace's pool. A workspace the set doesn't know (created
// after the set was built) is in the default pool, which is where a new
// workspace starts.
func (s *Set) PoolOf(workspaceID string) string {
	if p, ok := s.pools[workspaceID]; ok {
		return p
	}
	return relaypool.DefaultPoolID
}

// ForPool returns the list of a pool: its eligible relays, or an empty list
// when it has none (D-38: the relay path fails closed, never an implicit
// fallback to another pool).
func (s *Set) ForPool(poolID string) *pb.LabelledRelayList {
	if l, ok := s.lists[poolID]; ok {
		return l
	}
	return s.empty
}

// ForWorkspace returns the list a connector of workspaceID receives.
func (s *Set) ForWorkspace(workspaceID string) *pb.LabelledRelayList {
	return s.ForPool(s.PoolOf(workspaceID))
}
