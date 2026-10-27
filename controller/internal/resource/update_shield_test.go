package resource

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Sprint 20 Fix 18 (F24): updating a shield-enforced resource. Real Postgres
// (throwaway database); set RESOURCE_TEST_DATABASE_URL.
func TestUpdateShieldEnforcedResource(t *testing.T) {
	db := newResourceTestDB(t)
	ctx := context.Background()
	slug := fmt.Sprintf("f24-%d", time.Now().UnixNano())

	var tenantID, networkID, otherNetworkID, connectorID string
	mustScan := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	mustScan("workspace", db.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug, "ws-"+slug+".zecurity.in").Scan(&tenantID))
	mustScan("network", db.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net', 'other') RETURNING id`, tenantID).Scan(&networkID))
	mustScan("other network", db.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net2', 'other') RETURNING id`, tenantID).Scan(&otherNetworkID))
	mustScan("connector", db.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1, $2, 'conn', 'active') RETURNING id`,
		tenantID, networkID).Scan(&connectorID))
	shield := func(name, lan, status string) string {
		t.Helper()
		var id string
		mustScan("shield "+name, db.QueryRow(ctx,
			`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, lan_ip)
			 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			tenantID, networkID, connectorID, name, status, lan).Scan(&id))
		return id
	}
	s1 := shield("s1", "10.0.0.1", "active")
	s2 := shield("s2", "10.0.0.2", "active")
	shield("s3-revoked", "10.0.0.3", "revoked")
	s4 := shield("s4-offline", "10.0.0.4", "disconnected")
	shield("s5-pending", "10.0.0.5", "pending")

	port := 9400
	mk := func(status, action, host string, shieldID any) string {
		t.Helper()
		port++
		var id string
		mustScan("resource", db.QueryRow(ctx,
			`INSERT INTO resources (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
			 VALUES ($1, $2, $3, $4, $5, 'tcp', $6, $6, $7, $8) RETURNING id`,
			tenantID, networkID, shieldID, fmt.Sprintf("r-%d", port), host, port, status, action).Scan(&id))
		return id
	}
	type state struct{ status, action, host, shield string }
	stateOf := func(id string) state {
		t.Helper()
		var st state
		mustScan("read resource", db.QueryRow(ctx,
			`SELECT status, pending_action, host, COALESCE(shield_id::text, '') FROM resources WHERE id = $1`, id,
		).Scan(&st.status, &st.action, &st.host, &st.shield))
		return st
	}
	desired := func(shieldID, resourceID string) (bool, int, string) {
		t.Helper()
		rows, err := GetDesiredForShield(ctx, db, shieldID)
		if err != nil {
			t.Fatalf("desired %s: %v", shieldID, err)
		}
		for _, r := range rows {
			if r.ID == resourceID {
				return true, r.PortFrom, r.Host
			}
		}
		return false, 0, ""
	}
	str := func(s string) *string { return &s }
	num := func(i int) *int { return &i }
	pushes := func(res *UpdateResult) []string {
		var out []string
		for _, p := range res.ShieldPushes {
			if p.ConnectorID != connectorID {
				t.Errorf("push for shield %s names connector %s, want %s", p.ShieldID, p.ConnectorID, connectorID)
			}
			out = append(out, p.ShieldID)
		}
		return out
	}
	eq := func(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

	t.Run("unprotected: host edit applies, no shield work", func(t *testing.T) {
		id := mk("unprotected", "remove", "10.0.0.50", nil)
		res, err := Update(ctx, db, tenantID, id, UpdateInput{Host: str(" 10.0.0.51 ")})
		if err != nil {
			t.Fatal(err)
		}
		if res.Moved || len(res.ShieldPushes) != 0 {
			t.Fatalf("moved=%v pushes=%v, want none", res.Moved, res.ShieldPushes)
		}
		if st := stateOf(id); st.host != "10.0.0.51" || st.status != "unprotected" {
			t.Fatalf("state = %+v", st)
		}
	})

	t.Run("protected: port edit re-protects on its shield with the new port", func(t *testing.T) {
		id := mk("protected", "apply", "10.0.0.1", s1)
		res, err := Update(ctx, db, tenantID, id, UpdateInput{PortFrom: num(9555), PortTo: num(9555)})
		if err != nil {
			t.Fatal(err)
		}
		if res.Moved || !eq(pushes(res), []string{s1}) {
			t.Fatalf("moved=%v pushes=%v, want [s1]", res.Moved, pushes(res))
		}
		if st := stateOf(id); st.status != "protecting" || st.action != "apply" || st.shield != s1 {
			t.Fatalf("state = %+v, want protecting/apply on s1 until the shield acks", st)
		}
		if _, err := RecordAck(ctx, db, tenantID, id, "protected", "", time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		if st := stateOf(id); st.status != "protected" {
			t.Fatalf("after the shield's ack: %+v, want protected", st)
		}
		if in, p, _ := desired(s1, id); !in || p != 9555 {
			t.Fatalf("s1 desired: in=%v port=%d, want 9555", in, p)
		}
	})

	t.Run("failed counts as enforced: port edit re-pushes", func(t *testing.T) {
		id := mk("failed", "apply", "10.0.0.1", s1)
		res, err := Update(ctx, db, tenantID, id, UpdateInput{PortTo: num(9600)})
		if err != nil {
			t.Fatal(err)
		}
		if !eq(pushes(res), []string{s1}) {
			t.Fatalf("pushes=%v, want [s1]", pushes(res))
		}
		if st := stateOf(id); st.status != "protecting" {
			t.Fatalf("state = %+v, want protecting (re-verified by the shield)", st)
		}
	})

	t.Run("protected: name or description edit needs no push", func(t *testing.T) {
		id := mk("protected", "apply", "10.0.0.1", s1)
		res, err := Update(ctx, db, tenantID, id, UpdateInput{Name: str("renamed"), Description: str("d")})
		if err != nil {
			t.Fatal(err)
		}
		if res.Moved || len(res.ShieldPushes) != 0 {
			t.Fatalf("moved=%v pushes=%v, want none", res.Moved, res.ShieldPushes)
		}
		if st := stateOf(id); st.status != "protected" {
			t.Fatalf("state = %+v, want protected (no shield change)", st)
		}
	})

	t.Run("protected: host edit moves it to the shield on the new host", func(t *testing.T) {
		id := mk("protected", "apply", "10.0.0.1", s1)
		res, err := Update(ctx, db, tenantID, id, UpdateInput{Host: str("10.0.0.2")})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Moved || !eq(pushes(res), []string{s2, s1}) {
			t.Fatalf("moved=%v pushes=%v, want moved, [s2 s1]", res.Moved, pushes(res))
		}
		if st := stateOf(id); st.status != "protecting" || st.action != "apply" || st.shield != s2 || st.host != "10.0.0.2" {
			t.Fatalf("state = %+v, want protecting/apply on s2", st)
		}
		if in, _, h := desired(s2, id); !in || h != "10.0.0.2" {
			t.Fatalf("s2 desired: in=%v host=%s", in, h)
		}
		if in, _, _ := desired(s1, id); in {
			t.Fatal("s1 still has the moved resource in its desired set")
		}
		if res.Row.ShieldID != s2 {
			t.Fatalf("returned row shield = %s, want s2", res.Row.ShieldID)
		}
		// A late ack from s1 can't undo the move.
		if _, err := RecordAck(ctx, db, tenantID, id, "unprotected", "", time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		if st := stateOf(id); st.status != "protecting" || st.shield != s2 {
			t.Fatalf("after stray unprotected ack: %+v", st)
		}
	})

	t.Run("protected: host edit to an offline shield is accepted, applied on reconnect", func(t *testing.T) {
		id := mk("protected", "apply", "10.0.0.1", s1)
		res, err := Update(ctx, db, tenantID, id, UpdateInput{Host: str("10.0.0.4")})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Moved || !eq(pushes(res), []string{s4, s1}) {
			t.Fatalf("moved=%v pushes=%v, want moved, [s4 s1]", res.Moved, pushes(res))
		}
		if st := stateOf(id); st.status != "protecting" || st.shield != s4 {
			t.Fatalf("state = %+v, want protecting on s4", st)
		}
		if in, _, _ := desired(s4, id); !in {
			t.Fatal("offline shield's desired set (replayed on reconnect) lacks the resource")
		}
	})

	t.Run("protected: host with no enrolled shield is refused, nothing changes", func(t *testing.T) {
		for _, h := range []string{"10.0.0.9", "10.0.0.3", "10.0.0.5"} { // none; revoked; pending
			id := mk("protected", "apply", "10.0.0.1", s1)
			_, err := Update(ctx, db, tenantID, id, UpdateInput{Host: str(h), Name: str("x")})
			if !errors.Is(err, ErrNoMatchingShield) {
				t.Fatalf("host %s: err = %v, want ErrNoMatchingShield", h, err)
			}
			if st := stateOf(id); st.host != "10.0.0.1" || st.shield != s1 || st.status != "protected" {
				t.Fatalf("host %s: state changed: %+v", h, st)
			}
		}
	})

	t.Run("protected: network change needs a shield in the new network", func(t *testing.T) {
		id := mk("protected", "apply", "10.0.0.1", s1)
		_, err := Update(ctx, db, tenantID, id, UpdateInput{RemoteNetworkID: str(otherNetworkID)})
		if !errors.Is(err, ErrNoMatchingShield) {
			t.Fatalf("err = %v, want ErrNoMatchingShield", err)
		}
	})

	t.Run("protected: a UI save with unchanged values is no shield work", func(t *testing.T) {
		id := mk("protected", "apply", "10.0.0.1", s1)
		st := stateOf(id)
		var p int
		mustScan("port", db.QueryRow(ctx, `SELECT port_from FROM resources WHERE id = $1`, id).Scan(&p))
		res, err := Update(ctx, db, tenantID, id, UpdateInput{
			Name: str("renamed-ui"), Host: str(st.host), RemoteNetworkID: str(networkID),
			Protocol: str("tcp"), PortFrom: num(p), PortTo: num(p),
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Moved || len(res.ShieldPushes) != 0 {
			t.Fatalf("moved=%v pushes=%v, want none", res.Moved, res.ShieldPushes)
		}
	})

	t.Run("protect in flight (e.g. shield offline): a further edit is allowed", func(t *testing.T) {
		id := mk("protecting", "apply", "10.0.0.1", s1)
		res, err := Update(ctx, db, tenantID, id, UpdateInput{PortFrom: num(9777), PortTo: num(9777)})
		if err != nil {
			t.Fatal(err)
		}
		if !eq(pushes(res), []string{s1}) {
			t.Fatalf("pushes=%v, want [s1]", pushes(res))
		}
		if in, p, _ := desired(s1, id); !in || p != 9777 {
			t.Fatalf("s1 desired: in=%v port=%d, want 9777", in, p)
		}
	})

	t.Run("unprotect or delete in flight: shield-relevant edit refused, name allowed", func(t *testing.T) {
		for _, c := range []struct{ status, action string }{{"protecting", "remove"}, {"deleting", "remove"}} {
			id := mk(c.status, c.action, "10.0.0.1", s1)
			if _, err := Update(ctx, db, tenantID, id, UpdateInput{PortFrom: num(1)}); !errors.Is(err, ErrResourceBusy) {
				t.Fatalf("%s/%s port edit: err = %v, want ErrResourceBusy", c.status, c.action, err)
			}
			// A UI save sends every field: unchanged values are not a shield edit.
			if _, err := Update(ctx, db, tenantID, id, UpdateInput{
				Name: str("ok-" + c.status + "-" + c.action), Host: str("10.0.0.1"), Protocol: str("tcp"),
			}); err != nil {
				t.Fatalf("%s/%s name edit: %v", c.status, c.action, err)
			}
		}
	})

	t.Run("empty host refused", func(t *testing.T) {
		id := mk("unprotected", "remove", "10.0.0.60", nil)
		if _, err := Update(ctx, db, tenantID, id, UpdateInput{Host: str("  ")}); err == nil {
			t.Fatal("empty host accepted")
		}
	})
}
