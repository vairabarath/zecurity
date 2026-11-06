package main

import (
	"io"
	"testing"

	"github.com/yourorg/ztna/controller/internal/audit"
)

// PENDING-11: `zecurity-controller audit verify --workspace <id> [--stream s]`.
func TestAuditVerifyArgs(t *testing.T) {
	if !isAuditVerifyCommand([]string{"zecurity-controller", "audit", "verify"}) ||
		isAuditVerifyCommand([]string{"zecurity-controller", "audit"}) {
		t.Fatal("command detection")
	}
	ws := "6F1C0A52-2D4F-4B1E-9A51-0C1F5D0E7B11"
	a, err := parseAuditVerifyArgs([]string{"--workspace", ws}, io.Discard)
	if err != nil || a.workspace != "6f1c0a52-2d4f-4b1e-9a51-0c1f5d0e7b11" || len(a.streams) != 2 {
		t.Fatalf("both streams: %+v %v", a, err)
	}
	a, err = parseAuditVerifyArgs([]string{"--workspace", ws, "--stream", "access"}, io.Discard)
	if err != nil || len(a.streams) != 1 || a.streams[0] != audit.StreamAccess {
		t.Fatalf("one stream: %+v %v", a, err)
	}
	for _, bad := range [][]string{
		{},
		{"--workspace", "nope"},
		{"--workspace", ws, "--stream", "provider"},
		{"--workspace", ws, "extra"},
	} {
		if _, err := parseAuditVerifyArgs(bad, io.Discard); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
