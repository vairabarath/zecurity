package client

// PENDING-20 G1 (DB-backed; must run, not skip, in the gate): the client RPCs
// record the country of the gRPC peer and a move re-evaluates the device.

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/peer"

	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/geoip"
	"github.com/yourorg/ztna/controller/internal/geoip/geoiptest"
	"github.com/yourorg/ztna/controller/internal/posture"
)

func fromAddr(ctx context.Context, ip string) context.Context {
	return peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 51000}})
}

func TestLocationObservedAndMoveReevaluates(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := clientPostureTestDBName(t)
	adminPool := clientPostureTestPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()
	testDSN, err := clientPostureTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatal(err)
	}
	pool := clientPostureTestPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyClientPostureTestMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	ws := insertClientPostureWorkspace(t, ctx, pool, "geo")
	user := insertClientPostureUser(t, ctx, pool, ws, "geo")
	device := insertClientPostureDevice(t, ctx, pool, ws, user, "geo")

	store := posture.NewStore(pool)
	profile, err := store.CreateProfile(ctx, ws, "India only", true)
	if err != nil {
		t.Fatal(err)
	}
	rule, _ := posture.NormalizeCountryRule(posture.CountryRule{Allow: []string{"IN"}})
	if err := store.AddRequirement(ctx, ws, profile.ID, posture.Requirement{CheckID: posture.CheckNetworkCountry, Countries: &rule}); err != nil {
		t.Fatal(err)
	}

	geoDB, err := geoip.Open(geoiptest.WriteCountryDB(t, time.Now(), map[string]string{
		"49.36.0.0/16":  "IN",
		"36.110.0.0/16": "CN",
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	service := (&Service{
		pool: pool,
		authSvc: &postureTestAuth{claims: &auth.AccessTokenClaims{
			UserID: user.String(), TenantID: ws.String(), Role: "member",
		}},
		postureStore:     store,
		postureEvaluator: posture.NewEvaluator(store, nil),
	}).WithGeoIP(geoDB)

	satisfied := func() (bool, string) {
		t.Helper()
		ev, err := store.LatestEvaluation(ctx, ws, device, profile.ID)
		if err != nil {
			t.Fatalf("latest evaluation: %v", err)
		}
		reason := ""
		if ev.Reason != nil {
			reason = *ev.Reason
		}
		return ev.Satisfied, reason
	}
	observed := func() posture.NetworkObservation {
		t.Helper()
		o, err := store.LatestNetworkObservation(ctx, ws, device)
		if err != nil {
			t.Fatalf("observation: %v", err)
		}
		return o
	}

	// Posture report from India: recorded before evaluation, so it passes.
	if _, err := service.ReportDevicePosture(fromAddr(ctx, "49.36.200.7"), postureRequest(device, uuid.NewString())); err != nil {
		t.Fatalf("report: %v", err)
	}
	if ok, reason := satisfied(); !ok {
		t.Fatalf("reported from IN: not satisfied (%s)", reason)
	}
	if o := observed(); o.Country != "IN" || o.NetworkPrefix.String() != "49.36.200.0/24" {
		t.Fatalf("observation %+v", o)
	}

	// A poll from the same country inside the refresh window writes nothing.
	first := observed().ObservedAt
	if service.observeLocation(fromAddr(ctx, "49.36.1.1"), ws, device, false) {
		t.Fatal("same country reported as a change")
	}
	if !observed().ObservedAt.Equal(first) {
		t.Fatal("an unchanged poll inside the refresh window wrote the observation")
	}

	// A poll from China: change detected, device re-evaluated, access lost.
	if !service.observeLocation(fromAddr(ctx, "36.110.5.5"), ws, device, false) {
		t.Fatal("IN→CN not reported as a change")
	}
	service.reevaluateAfterMove(ctx, ws, device)
	if ok, reason := satisfied(); ok || reason != "check zecurity.network.country: CN is not allowed" {
		t.Fatalf("after move to CN: satisfied=%v reason=%q", ok, reason)
	}

	// An address with no country (private) is unknown and denies.
	if !service.observeLocation(fromAddr(ctx, "10.1.2.3"), ws, device, false) {
		t.Fatal("CN→unknown not reported as a change")
	}
	service.reevaluateAfterMove(ctx, ws, device)
	if ok, _ := satisfied(); ok {
		t.Fatal("unknown location satisfied the rule")
	}

	// Without a GeoIP database configured, nothing is recorded (rules deny).
	plain := &Service{pool: pool, postureStore: store}
	if plain.observeLocation(fromAddr(ctx, "49.36.200.7"), ws, device, true) {
		t.Fatal("service without GeoIP recorded a sighting")
	}
}
