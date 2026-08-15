package connector

import (
	"reflect"
	"testing"

	"github.com/yourorg/ztna/controller/internal/resource"
)

// No database required: the whole point of extracting instructionFor was to make
// the PendingRow → ResourceInstruction mapping testable without one. Before this,
// the three construction sites were reachable only through a live snapshot build.

func pendingRow() *resource.PendingRow {
	return &resource.PendingRow{
		ID:            "res-1",
		Host:          "10.0.0.7",
		Protocol:      "tcp",
		PortFrom:      8080,
		PortTo:        8090,
		PendingAction: "apply",
		LocalTarget:   "127.0.0.1",
	}
}

// The guard that matters. Every exported field on the wire message must be
// populated from a fully-populated row — so adding a proto field forces a
// deliberate decision here instead of shipping a silent empty value on all three
// delivery paths. This is the test that would have failed if Phase 8 had added
// local_target to the proto and forgotten one of the sites.
func TestInstructionForMapsEveryWireField(t *testing.T) {
	row := pendingRow()
	instr := instructionFor(row, row.PendingAction)

	v := reflect.ValueOf(instr).Elem()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue // unexported protoimpl bookkeeping (state, sizeCache, ...)
		}
		if v.Field(i).IsZero() {
			t.Errorf("ResourceInstruction.%s is zero — every wire field must be mapped "+
				"in instructionFor. A newly added proto field needs a deliberate "+
				"decision here, not a silent empty value on every delivery path.",
				field.Name)
		}
	}
}

func TestInstructionForCopiesValues(t *testing.T) {
	row := pendingRow()
	instr := instructionFor(row, row.PendingAction)

	if instr.ResourceId != "res-1" {
		t.Errorf("ResourceId = %q, want res-1", instr.ResourceId)
	}
	if instr.Host != "10.0.0.7" {
		t.Errorf("Host = %q, want 10.0.0.7", instr.Host)
	}
	if instr.Protocol != "tcp" {
		t.Errorf("Protocol = %q, want tcp", instr.Protocol)
	}
	if instr.PortFrom != 8080 || instr.PortTo != 8090 {
		t.Errorf("ports = %d-%d, want 8080-8090", instr.PortFrom, instr.PortTo)
	}
	if instr.Action != "apply" {
		t.Errorf("Action = %q, want apply", instr.Action)
	}
	if instr.LocalTarget != "127.0.0.1" {
		t.Errorf("LocalTarget = %q, want 127.0.0.1", instr.LocalTarget)
	}
}

// Snapshot semantics: a snapshot lists desired state, so there is no per-row
// action. The shield treats every listed row as "enforce this"; sending "apply"
// here would make the snapshot look like a batch of incremental instructions.
func TestInstructionForSnapshotCarriesNoAction(t *testing.T) {
	instr := instructionFor(pendingRow(), "")
	if instr.Action != "" {
		t.Errorf("Action = %q, want empty for a snapshot row", instr.Action)
	}
	// The rest of the payload must still be intact — an empty action is not an
	// empty instruction.
	if instr.LocalTarget != "127.0.0.1" || instr.Host != "10.0.0.7" {
		t.Errorf("snapshot row lost payload: host=%q local_target=%q",
			instr.Host, instr.LocalTarget)
	}
}

func TestInstructionForRowDerefsLocalTarget(t *testing.T) {
	lt := "127.0.0.1"
	instr := instructionForRow(&resource.Row{
		ID:            "res-2",
		Host:          "10.0.0.8",
		Protocol:      "tcp",
		PortFrom:      443,
		PortTo:        443,
		PendingAction: "apply",
		LocalTarget:   &lt,
	})
	if instr.LocalTarget != "127.0.0.1" {
		t.Errorf("LocalTarget = %q, want 127.0.0.1", instr.LocalTarget)
	}
	if instr.ResourceId != "res-2" || instr.Action != "apply" {
		t.Errorf("row fields lost: id=%q action=%q", instr.ResourceId, instr.Action)
	}
}

// A NULL local_target is the common case — every resource created before Phase 8
// has one. It must flatten to "" (dial Host) and never panic.
func TestInstructionForRowHandlesNilLocalTarget(t *testing.T) {
	instr := instructionForRow(&resource.Row{
		ID:            "res-3",
		Host:          "10.0.0.9",
		Protocol:      "tcp",
		PortFrom:      22,
		PortTo:        22,
		PendingAction: "remove",
		LocalTarget:   nil,
	})
	if instr.LocalTarget != "" {
		t.Errorf("LocalTarget = %q, want empty for a NULL column", instr.LocalTarget)
	}
	if instr.Host != "10.0.0.9" {
		t.Errorf("Host = %q, want 10.0.0.9", instr.Host)
	}
}
