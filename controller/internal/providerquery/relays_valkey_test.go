package providerquery_test

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/providerquery"
	"github.com/yourorg/ztna/controller/internal/relay"
)

// AT-R.2 against a real Valkey through relay.Service.LastHeartbeat, the
// reader production wires in: the DB heartbeat is 4 minutes old (inside the
// 5-minute write throttle) and the liveness key is fresh. External test
// package: importing internal/relay from the internal one would be a cycle
// (relay → provider → providerquery).
func TestListRelays_LivenessPrefersFreshValkey(t *testing.T) {
	pool := providerquery.NewTestDB(t)
	rdb := newTestValkey(t)
	ctx := context.Background()

	stale := time.Now().UTC().Add(-4 * time.Minute).Truncate(time.Second)
	fresh := time.Now().UTC().Truncate(time.Second)
	id := providerquery.SeedRelay(t, pool, "live-relay", "active", providerquery.TestT0)
	providerquery.MustExec(t, pool, `UPDATE relays SET last_heartbeat_at=$2 WHERE id=$1`, id, stale)
	key := "relay:heartbeat:last:" + id
	if err := rdb.Set(ctx, key, strconv.FormatInt(fresh.Unix(), 10), 330*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })

	hb := relay.NewService(nil, nil, 0).WithHeartbeatCache(rdb, 0)
	page, err := providerquery.ListRelays(ctx, pool, hb, providerquery.RelayFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !page.Items[0].LastHeartbeatAt.Equal(fresh) || page.Items[0].Status != "active" {
		t.Fatalf("list liveness = %+v, want fresh %v and active", page.Items, fresh)
	}
	d, err := providerquery.GetRelay(ctx, pool, hb, id)
	if err != nil || !d.LastHeartbeatAt.Equal(fresh) {
		t.Fatalf("detail liveness = %v (%v), want %v", d.LastHeartbeatAt, err, fresh)
	}

	// Key expired or absent: the DB value stands.
	_ = rdb.Del(ctx, key).Err()
	page, err = providerquery.ListRelays(ctx, pool, hb, providerquery.RelayFilter{})
	if err != nil || !page.Items[0].LastHeartbeatAt.Equal(stale) {
		t.Fatalf("without key = %v (%v), want %v", page.Items[0].LastHeartbeatAt, err, stale)
	}
}

func newTestValkey(t *testing.T) valkeycompat.Cmdable {
	t.Helper()
	raw := os.Getenv("AUTH_TEST_VALKEY_URL")
	if raw == "" {
		t.Skip("AUTH_TEST_VALKEY_URL not set")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("bad AUTH_TEST_VALKEY_URL %q: %v", raw, err)
	}
	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{u.Host}})
	if err != nil {
		t.Fatalf("valkey client: %v", err)
	}
	t.Cleanup(client.Close)
	return valkeycompat.NewAdapter(client)
}
