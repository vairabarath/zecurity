package policy

// acl_wire_contract_test.go -- PENDING-16 Phase 9.
//
// Guards one architectural property of the Sprint 19 cutover: Device Profiles
// are a Controller-side concept and must never reach the Connector.
//
// The ACL compiler resolves Resource -> Resource Policy -> Device Profile(s) ->
// posture entirely on the Controller and flattens the answer into
// ACLEntry.allowed_spiffe_ids. The Connector then does a membership test and
// nothing more (connector/src/policy/mod.rs is_allowed). If a profile identifier
// were ever added to the wire contract, that would move authorization logic onto
// the data plane -- the Connector would have to know what a profile is, evaluate
// it, and stay in step with the Controller's posture semantics.
//
// Today the property holds only by nobody having edited the proto. Nothing fails
// if someone adds `repeated string profile_ids` to ACLEntry. This test is that
// missing guard.

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

// TestACLWireContractCarriesNoDeviceProfileIdentity walks the ACL messages the
// Connector actually receives and fails if any field mentions a profile.
//
// Checked by name rather than against a fixed field list: a fixed list would
// also fail on unrelated additions (a new route hint, say), and this is a
// statement about profiles specifically, not about the message being frozen.
func TestACLWireContractCarriesNoDeviceProfileIdentity(t *testing.T) {
	messages := []struct {
		name       string
		descriptor protoreflect.MessageDescriptor
	}{
		{"ACLSnapshot", (&clientv1.ACLSnapshot{}).ProtoReflect().Descriptor()},
		{"ACLEntry", (&clientv1.ACLEntry{}).ProtoReflect().Descriptor()},
		{"ACLConnector", (&clientv1.ACLConnector{}).ProtoReflect().Descriptor()},
		{"ACLRemoteNetwork", (&clientv1.ACLRemoteNetwork{}).ProtoReflect().Descriptor()},
	}

	for _, message := range messages {
		fields := message.descriptor.Fields()

		for i := 0; i < fields.Len(); i++ {
			field := fields.Get(i)
			name := string(field.Name())

			if strings.Contains(strings.ToLower(name), "profile") {
				t.Errorf(
					"%s.%s: device profile identity must not appear in the ACL wire "+
						"contract -- posture is resolved Controller-side into "+
						"allowed_spiffe_ids, and the Connector must stay a membership "+
						"test (PENDING-16 Phase 5)",
					message.name,
					name,
				)
			}
		}
	}
}

// TestACLEntryCarriesTheAllowedIdentitySet is the positive half: the flattened
// SPIFFE list is what replaces profile identity on the wire, so its absence or
// renaming would break the same property from the other direction.
func TestACLEntryCarriesTheAllowedIdentitySet(t *testing.T) {
	fields := (&clientv1.ACLEntry{}).ProtoReflect().Descriptor().Fields()

	field := fields.ByName("allowed_spiffe_ids")
	if field == nil {
		t.Fatal("ACLEntry.allowed_spiffe_ids is missing: it is the compiled answer " +
			"the Connector enforces in place of any profile evaluation")
	}

	if !field.IsList() || field.Kind() != protoreflect.StringKind {
		t.Fatalf(
			"ACLEntry.allowed_spiffe_ids = %s (list=%t), want a repeated string",
			field.Kind(),
			field.IsList(),
		)
	}
}
