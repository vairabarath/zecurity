package relay

import (
	"context"
	"net/http"
	"testing"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// Relay link capacity (Sprint 23 B2, D-42; D-31 amended): relay-ops and
// super-admin set it with a reason; audited before/after; null clears;
// invalid values and terminal relays are refused.

func (e *adminEnv) capacity(t *testing.T, relayID string) *int64 {
	t.Helper()
	var v *int64
	if err := e.pool.QueryRow(context.Background(), `SELECT link_capacity_bps FROM relays WHERE id = $1`, relayID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func capPath(id string) string { return "/provider/relays/" + id + "/link-capacity" }

func TestLinkCapacity_SetClearAndAudit(t *testing.T) {
	e := newAdminEnv(t, false)
	relayID := e.eligibleRelay(t, "relay-cap")

	// relay-ops may set it (D-31 amended).
	if r := e.do(t, provider.RoleRelayOps, http.MethodPut, capPath(relayID), map[string]any{"bps": 125_000_000, "reason": "1 Gbps uplink"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("relay-ops set = %d %s", r.code, r.body)
	}
	if v := e.capacity(t, relayID); v == nil || *v != 125_000_000 {
		t.Fatalf("stored capacity %v, want 125000000", v)
	}
	rows := e.auditRows(t, provider.ActionRelayCapacitySet)
	if len(rows) != 1 || rows[0]["email"] != "ops@provider.test" || rows[0]["target"] != relayID {
		t.Fatalf("audit rows %v", rows)
	}
	d := rows[0]["details"].(map[string]any)
	if d["reason"] != "1 Gbps uplink" || d["before"].(map[string]any)["link_capacity_bps"] != nil ||
		d["after"].(map[string]any)["link_capacity_bps"] != float64(125_000_000) {
		t.Fatalf("audit details %v", d)
	}

	// The same value again is a no-op: not audited.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, capPath(relayID), map[string]any{"bps": 125_000_000, "reason": "again"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("repeat = %d %s", r.code, r.body)
	}
	if n := len(e.auditRows(t, provider.ActionRelayCapacitySet)); n != 1 {
		t.Fatalf("an unchanged value wrote an audit row (%d rows)", n)
	}

	// null clears it ("Not configured"), audited.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, capPath(relayID), map[string]any{"bps": nil, "reason": "unknown link"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("clear = %d %s", r.code, r.body)
	}
	if v := e.capacity(t, relayID); v != nil {
		t.Fatalf("cleared capacity %v, want null", *v)
	}
	if rows := e.auditRows(t, provider.ActionRelayCapacitySet); len(rows) != 2 ||
		rows[1]["details"].(map[string]any)["after"].(map[string]any)["link_capacity_bps"] != nil {
		t.Fatalf("clear audit %v", rows)
	}
}

func TestLinkCapacity_Validation(t *testing.T) {
	e := newAdminEnv(t, false)
	relayID := e.eligibleRelay(t, "relay-cap")
	cases := []struct {
		name string
		body any
		code string
	}{
		{"missing bps", map[string]any{"reason": "r"}, "invalid_request"},
		{"zero", map[string]any{"bps": 0, "reason": "r"}, "invalid_capacity"},
		{"negative", map[string]any{"bps": -5, "reason": "r"}, "invalid_capacity"},
		{"above the 400 Gbit/s bound", map[string]any{"bps": MaxLinkCapacityBps + 1, "reason": "r"}, "invalid_capacity"},
		{"fraction", `{"bps": 1.5, "reason": "r"}`, "invalid_capacity"},
		{"string", `{"bps": "1Gbps", "reason": "r"}`, "invalid_capacity"},
		{"no reason", map[string]any{"bps": 1000}, "reason_required"},
		{"unknown field", `{"bps": 1000, "reason": "r", "extra": 1}`, "invalid_request"},
	}
	for _, c := range cases {
		r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, capPath(relayID), c.body, nil)
		if r.code != http.StatusBadRequest || errorCode(t, r.body) != c.code {
			t.Errorf("%s = %d %s, want 400 %s", c.name, r.code, r.body, c.code)
		}
	}
	if v := e.capacity(t, relayID); v != nil {
		t.Fatalf("a refused request stored %d", *v)
	}
	if n := len(e.auditRows(t, provider.ActionRelayCapacitySet)); n != 0 {
		t.Fatalf("refused requests wrote %d audit rows", n)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, capPath(relayID), map[string]any{"bps": MaxLinkCapacityBps, "reason": "r"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("the bound itself = %d %s", r.code, r.body)
	}
}

func TestLinkCapacity_TerminalAndUnknownRelays(t *testing.T) {
	e := newAdminEnv(t, false)
	revoked := e.eligibleRelay(t, "relay-revoked")
	deleted := e.eligibleRelay(t, "relay-deleted")
	e.exec(t, `UPDATE relays SET status = 'revoked' WHERE id = $1`, revoked)
	e.exec(t, `UPDATE relays SET status = 'deleted' WHERE id = $1`, deleted)
	body := map[string]any{"bps": 1000, "reason": "r"}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, capPath(revoked), body, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "already_revoked" {
		t.Fatalf("revoked = %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, capPath(deleted), body, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "already_deleted" {
		t.Fatalf("deleted = %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, capPath("00000000-0000-0000-0000-0000000000ee"), body, nil); r.code != http.StatusNotFound {
		t.Fatalf("unknown relay = %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, capPath("not-a-uuid"), body, nil); r.code != http.StatusNotFound {
		t.Fatalf("bad id = %d %s", r.code, r.body)
	}
}
