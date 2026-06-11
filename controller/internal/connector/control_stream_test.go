package connector

import (
	"testing"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
)

// TestConnectorSendFailsFastWhenQueueFull asserts the F14 liveness guarantee:
// send() enqueues into the outbound mailbox and, when the mailbox is full (a
// connector that has stopped draining its stream), returns an error immediately
// instead of blocking the caller. The writer goroutine — not send — is the only
// thing that may block on stream.Send.
func TestConnectorSendFailsFastWhenQueueFull(t *testing.T) {
	c := &connectorStreamClient{
		outbound:    make(chan *pb.ConnectorControlMessage, 1),
		connectorID: "c1",
	}

	if err := c.send(&pb.ConnectorControlMessage{}); err != nil {
		t.Fatalf("first send should enqueue into an empty mailbox: %v", err)
	}
	if err := c.send(&pb.ConnectorControlMessage{}); err == nil {
		t.Fatal("send into a full mailbox must fail fast, not block")
	}

	// Draining one slot frees capacity for another non-blocking send.
	<-c.outbound
	if err := c.send(&pb.ConnectorControlMessage{}); err != nil {
		t.Fatalf("send should enqueue again after the mailbox drains: %v", err)
	}
}
