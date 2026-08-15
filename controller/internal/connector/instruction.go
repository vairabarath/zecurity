package connector

import (
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/resource"
)

// instructionFor is the single place a resource row becomes a wire
// ResourceInstruction.
//
// It exists because there are three delivery paths — the full snapshot, the
// single incremental push, and the reconnect remove-batch — and each used to
// build the message by hand. Sprint 16 Phase 8 called those three sites "the
// whole risk of this task": a proto field populated at two of three sites
// produces a resource that works after a full resync but not after an
// incremental push, which is a miserable bug to chase. Routing every path
// through one converter makes that divergence unrepresentable rather than
// merely discouraged.
//
// action is "" for snapshot rows: a snapshot has no per-row action, because
// every row it lists means "enforce this". The remove-batch and the single
// push pass the row's pending_action.
func instructionFor(r *resource.PendingRow, action string) *shieldpb.ResourceInstruction {
	return &shieldpb.ResourceInstruction{
		ResourceId:  r.ID,
		Host:        r.Host,
		Protocol:    r.Protocol,
		PortFrom:    int32(r.PortFrom),
		PortTo:      int32(r.PortTo),
		Action:      action,
		LocalTarget: r.LocalTarget,
	}
}

// instructionForRow adapts resource.Row for the single-push path.
//
// Row.LocalTarget is a nullable *string — the desired-set query COALESCEs the
// column, a full row read does not — so the nil case is flattened here to the
// same empty string the snapshot path produces. Empty means "dial Host" on the
// shield, which is the pre-Phase-8 behaviour.
func instructionForRow(row *resource.Row) *shieldpb.ResourceInstruction {
	localTarget := ""
	if row.LocalTarget != nil {
		localTarget = *row.LocalTarget
	}
	return instructionFor(&resource.PendingRow{
		ID:          row.ID,
		Host:        row.Host,
		Protocol:    row.Protocol,
		PortFrom:    row.PortFrom,
		PortTo:      row.PortTo,
		LocalTarget: localTarget,
	}, row.PendingAction)
}
