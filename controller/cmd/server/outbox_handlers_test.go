package main

import (
	"testing"

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/outbox"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// Every event type the controller enqueues must have a consumer registered at
// startup: an event without a handler is failed terminally on its first
// attempt, so a missing registration would silently drop the work (certificate
// audit bugs 4/5 add EventPolicyInvalidationRequested).
func TestRegisterOutboxHandlers_RegistersEveryProducedEventType(t *testing.T) {
	reg := outbox.NewHandlerRegistry()
	if err := registerOutboxHandlers(reg, nil, policy.NewNotifier(policy.NewSnapshotCache())); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, eventType := range []string{
		identity.EventDeviceTrustRevokeRequested,
		identity.EventDeviceTrustReEnrollmentRequired,
		policy.EventPolicyInvalidationRequested,
	} {
		if !reg.Has(eventType) {
			t.Errorf("no handler registered for %s", eventType)
		}
	}
}
