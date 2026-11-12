package provider_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yourorg/ztna/controller/internal/relay/relaypool"
)

// Relay pool reads (D-37 … D-41): relay-ops see pools with counts only, never
// tenant names (D-41, D-06); super-admins also get the tenant list. Relays and
// tenant detail name their pool.

func TestRelayPoolReads_CountsForRelayOpsTenantsForSuperAdmin(t *testing.T) {
	e := newReadEnv(t)

	r := e.get(t, "/provider/relay-pools", e.opsTok)
	if r.code != 200 {
		t.Fatalf("relay-ops list = %d %s", r.code, r.body)
	}
	var list struct {
		Items []struct {
			ID        string `json:"id"`
			IsDefault bool   `json:"is_default"`
			Relays    int    `json:"relays"`
			Tenants   int    `json:"tenants"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.body), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list body %s: %v", r.body, err)
	}
	if d := list.Items[0]; d.ID != relaypool.DefaultPoolID || !d.IsDefault || d.Relays != 1 || d.Tenants != 1 {
		t.Fatalf("default pool summary %+v, want default with 1 relay and 1 tenant", d)
	}

	detail := "/provider/relay-pools/" + relaypool.DefaultPoolID
	ops := e.get(t, detail, e.opsTok)
	if ops.code != 200 {
		t.Fatalf("relay-ops detail = %d %s", ops.code, ops.body)
	}
	var opsBody map[string]any
	_ = json.Unmarshal([]byte(ops.body), &opsBody)
	if opsBody["tenant_list"] != nil {
		t.Fatalf("relay-ops got a tenant list: %v", opsBody["tenant_list"])
	}
	// The seeded tenant is slug "acme", name "Acme": neither may reach relay-ops.
	if strings.Contains(strings.ToLower(ops.body), "acme") {
		t.Fatalf("relay-ops pool detail leaks a tenant name: %s", ops.body)
	}
	if opsBody["tenants"] != float64(1) {
		t.Fatalf("relay-ops tenant count %v, want 1", opsBody["tenants"])
	}

	super := e.get(t, detail, e.superTok)
	var superBody struct {
		TenantList *[]struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"tenant_list"`
	}
	if err := json.Unmarshal([]byte(super.body), &superBody); err != nil || superBody.TenantList == nil ||
		len(*superBody.TenantList) != 1 || (*superBody.TenantList)[0].ID != e.tenantID || (*superBody.TenantList)[0].Slug != "acme" {
		t.Fatalf("super-admin tenant list %s: %v", super.body, err)
	}

	if r := e.get(t, "/provider/relay-pools/not-a-uuid", e.superTok); r.code != 400 || readErrCode(t, r) != "invalid_id" {
		t.Fatalf("bad id = %d %s", r.code, r.body)
	}
	if r := e.get(t, "/provider/relay-pools/00000000-0000-0000-0000-0000000000ff", e.superTok); r.code != 404 {
		t.Fatalf("unknown pool = %d %s", r.code, r.body)
	}
}

func TestRelayPoolReads_RelayAndTenantNameTheirPool(t *testing.T) {
	e := newReadEnv(t)
	r := e.get(t, "/provider/relays/"+e.relayID, e.opsTok)
	var relay struct {
		Pool struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"pool"`
	}
	if err := json.Unmarshal([]byte(r.body), &relay); err != nil || relay.Pool.ID != relaypool.DefaultPoolID || relay.Pool.Name != "default" {
		t.Fatalf("relay pool %s: %v", r.body, err)
	}

	type tenantPool struct {
		RelayPool struct {
			ID       string `json:"id"`
			Previous *struct {
				ID    string `json:"id"`
				Until string `json:"until"`
			} `json:"previous"`
		} `json:"relay_pool"`
	}
	read := func() tenantPool {
		t.Helper()
		r := e.get(t, "/provider/tenants/"+e.tenantID, e.superTok)
		var tp tenantPool
		if err := json.Unmarshal([]byte(r.body), &tp); err != nil || r.code != 200 {
			t.Fatalf("tenant detail %d %s: %v", r.code, r.body, err)
		}
		return tp
	}
	if tp := read(); tp.RelayPool.ID != relaypool.DefaultPoolID || tp.RelayPool.Previous != nil {
		t.Fatalf("tenant pool %+v, want default and no previous", tp.RelayPool)
	}
	// A graceful migration in progress shows the previous pool until it ends.
	next := scalar[string](t, e.pool, `INSERT INTO relay_pools (name) VALUES ('next') RETURNING id::text`)
	exec(t, e.pool, `UPDATE workspaces SET relay_pool_id = $2, relay_pool_previous_id = $3,
	                  relay_pool_previous_until = now() + interval '5 minutes' WHERE id = $1`, e.tenantID, next, relaypool.DefaultPoolID)
	if tp := read(); tp.RelayPool.ID != next || tp.RelayPool.Previous == nil || tp.RelayPool.Previous.ID != relaypool.DefaultPoolID {
		t.Fatalf("tenant pool during migration %+v", tp.RelayPool)
	}
	exec(t, e.pool, `UPDATE workspaces SET relay_pool_previous_until = now() - interval '1 second' WHERE id = $1`, e.tenantID)
	if tp := read(); tp.RelayPool.Previous != nil {
		t.Fatalf("expired migration still shown: %+v", tp.RelayPool.Previous)
	}
}

// D-39: tenant detail names the failover pool and whether it is in use;
// pools show their failover state and how many tenants fail over to them.
func TestRelayPoolReads_FailoverState(t *testing.T) {
	e := newReadEnv(t)
	fo := scalar[string](t, e.pool, `INSERT INTO relay_pools (name) VALUES ('backup') RETURNING id::text`)
	exec(t, e.pool, `UPDATE workspaces SET relay_failover_pool_id = $2 WHERE id = $1`, e.tenantID, fo)

	type tenantPool struct {
		RelayPool struct {
			Failover *struct {
				ID string `json:"id"`
			} `json:"failover"`
			UsingFailover   bool    `json:"using_failover"`
			FailedOverSince *string `json:"failed_over_since"`
		} `json:"relay_pool"`
	}
	read := func() tenantPool {
		t.Helper()
		r := e.get(t, "/provider/tenants/"+e.tenantID, e.superTok)
		var tp tenantPool
		if err := json.Unmarshal([]byte(r.body), &tp); err != nil || r.code != 200 {
			t.Fatalf("tenant detail %d %s: %v", r.code, r.body, err)
		}
		return tp
	}
	if tp := read(); tp.RelayPool.Failover == nil || tp.RelayPool.Failover.ID != fo || tp.RelayPool.UsingFailover {
		t.Fatalf("healthy primary with a failover pool: %+v", tp.RelayPool)
	}
	exec(t, e.pool, `UPDATE relay_pools SET exhausted_at = now() WHERE id = $1`, relaypool.DefaultPoolID)
	if tp := read(); !tp.RelayPool.UsingFailover || tp.RelayPool.FailedOverSince == nil {
		t.Fatalf("exhausted primary: %+v, want using_failover with a since time", tp.RelayPool)
	}

	r := e.get(t, "/provider/relay-pools", e.opsTok)
	var list struct {
		Items []struct {
			ID              string `json:"id"`
			State           string `json:"state"`
			FailoverTenants int    `json:"failover_tenants"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.body), &list); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range list.Items {
		got[p.ID] = p.State
		if p.ID == fo && p.FailoverTenants != 1 {
			t.Fatalf("failover pool failover_tenants %d, want 1", p.FailoverTenants)
		}
	}
	if got[relaypool.DefaultPoolID] != relaypool.StateFailedOver || got[fo] != relaypool.StateHealthy {
		t.Fatalf("pool states %v", got)
	}
}
