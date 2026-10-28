package connector

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
)

// FU-3: a scan command reaches a connector only if the connector's live
// stream belongs to the caller's workspace. The stream's tenant comes from the
// authenticated connector record (Control), never from the caller. Fake
// streams only: nothing is scanned.
func TestPushScanCommand_WorkspaceScoped(t *testing.T) {
	r := NewConnectorRegistry()
	connA := &connectorStreamClient{outbound: make(chan *pb.ConnectorControlMessage, 4), connectorID: "conn-a", tenantID: "ws-A"}
	connB := &connectorStreamClient{outbound: make(chan *pb.ConnectorControlMessage, 4), connectorID: "conn-b", tenantID: "ws-B"}
	r.add(connA.connectorID, connA)
	r.add(connB.connectorID, connB)
	msg := func() *pb.ConnectorControlMessage {
		return &pb.ConnectorControlMessage{Body: &pb.ConnectorControlMessage_ScanCommand{ScanCommand: &pb.ScanCommand{RequestId: "req"}}}
	}

	// Positive control: A's own connected connector receives the command.
	if err := r.PushScanCommand("ws-A", "conn-a", msg()); err != nil {
		t.Fatalf("own connector: %v", err)
	}
	if len(connA.outbound) != 1 {
		t.Fatalf("own connector got %d messages, want 1", len(connA.outbound))
	}

	// Foreign, missing and malformed connectors: the same error, and nothing
	// is sent anywhere.
	for _, c := range []struct{ name, id string }{
		{"foreign connector (connected)", "conn-b"},
		{"missing connector", "conn-missing"},
		{"malformed id", ""},
	} {
		err := r.PushScanCommand("ws-A", c.id, msg())
		if !errors.Is(err, ErrConnectorNotAvailable) {
			t.Errorf("%s: err = %v, want ErrConnectorNotAvailable", c.name, err)
		}
	}
	if len(connB.outbound) != 0 {
		t.Errorf("B's connector got %d messages from A's attempts, want 0", len(connB.outbound))
	}
	if len(connA.outbound) != 1 {
		t.Errorf("A's connector got extra messages: %d", len(connA.outbound))
	}

	// B can still scan through its own connector.
	if err := r.PushScanCommand("ws-B", "conn-b", msg()); err != nil || len(connB.outbound) != 1 {
		t.Errorf("B's own connector: err=%v messages=%d", err, len(connB.outbound))
	}
}

// A refused dispatch leaves a server-side log line with the real reason (the
// client always gets the same ErrConnectorNotAvailable). The client-supplied
// connector ID is quoted and truncated, so it can't forge log lines.
func TestPushScanCommand_RefusalsAreLogged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	r := NewConnectorRegistry()
	connA := &connectorStreamClient{outbound: make(chan *pb.ConnectorControlMessage, 4), connectorID: "conn-a", tenantID: "ws-A"}
	connB := &connectorStreamClient{outbound: make(chan *pb.ConnectorControlMessage, 4), connectorID: "conn-b", tenantID: "ws-B"}
	r.add(connA.connectorID, connA)
	r.add(connB.connectorID, connB)
	msg := &pb.ConnectorControlMessage{Body: &pb.ConnectorControlMessage_ScanCommand{ScanCommand: &pb.ScanCommand{RequestId: "req", Targets: []string{"10.0.0.0/30"}}}}

	// Legitimate dispatch: delivered, nothing logged as refused.
	if err := r.PushScanCommand("ws-A", "conn-a", msg); err != nil {
		t.Fatalf("own connector: %v", err)
	}
	if strings.Contains(buf.String(), "scan dispatch refused") {
		t.Fatalf("legitimate dispatch logged as refused: %q", buf.String())
	}

	cases := []struct{ name, id, reason string }{
		{"foreign connector", "conn-b", "reason=foreign_workspace connector_workspace=ws-B"},
		{"unknown connector", "conn-missing", "reason=not_connected"},
		{"forged log line", "x\n2026/10/27 security: fake entry", "reason=not_connected"},
	}
	for _, c := range cases {
		buf.Reset()
		err := r.PushScanCommand("ws-A", c.id, msg)
		if err != ErrConnectorNotAvailable {
			t.Errorf("%s: err = %v, want exactly ErrConnectorNotAvailable", c.name, err)
		}
		line := buf.String()
		if !strings.Contains(line, "security: scan dispatch refused: workspace=ws-A") || !strings.Contains(line, c.reason) {
			t.Errorf("%s: log %q, want the caller workspace and %q", c.name, line, c.reason)
		}
		if strings.Count(line, "\n") != 1 {
			t.Errorf("%s: refusal produced %d log lines, want 1: %q", c.name, strings.Count(line, "\n"), line)
		}
		if strings.Contains(line, "10.0.0.0/30") {
			t.Errorf("%s: log carries the scan targets: %q", c.name, line)
		}
	}
	if len(connB.outbound) != 0 {
		t.Errorf("B's connector received %d message(s) from refused dispatches", len(connB.outbound))
	}
	if len(connA.outbound) != 1 {
		t.Errorf("A's connector has %d messages, want 1 (the legitimate one)", len(connA.outbound))
	}

	// Over-long IDs are truncated in the log.
	buf.Reset()
	_ = r.PushScanCommand("ws-A", strings.Repeat("z", 500), msg)
	if strings.Count(buf.String(), "z") > 80 {
		t.Errorf("long connector id not truncated in the log: %d bytes", buf.Len())
	}
}
