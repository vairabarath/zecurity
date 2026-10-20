//go:build xlangharness

// Command xlangharness serves the PRODUCTION connector Control handler
// (EnrollmentHandler.Control) over a real grpc-go server for the
// cross-language authorization-marker test (connector/tests/
// xlang_authz_markers.rs). It exists only for that test: it is excluded from
// normal builds by the xlangharness build tag and is never shipped.
//
// Test-only differences from production, and why they don't affect what is
// under test:
//   - plaintext h2c instead of mTLS, and the SPIFFE identity is injected from
//     the "x-xlang-connector-id" request header by a test interceptor (the
//     real interceptor needs a client certificate). Control, the marker
//     encoding, status codes and trailers are the production code paths.
//   - a throwaway database (created + migrated here, dropped on exit).
//
// Protocol: prints one JSON line with the listen address and seeded ids, then
// reads commands on stdin, answering "ok <cmd>" or "err <cmd>: <reason>":
//
//	revoke <id>   UPDATE … status='revoked' + Registry.SignalRevoked (as RevokeConnector)
//	delete <id>   DELETE the connector row
//	dbdown        close the pool (every lookup fails afterwards)
//	quit          drop the database and exit (also on stdin EOF)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/connector"
	"github.com/yourorg/ztna/controller/internal/spiffe"
)

type seeded struct {
	Addr        string `json:"addr"`
	TrustDomain string `json:"trust_domain"`
	Active      string `json:"active"`      // ack + live revoke
	ActiveDel   string `json:"active_del"`  // ack + delete mid-stream
	ActiveZero  string `json:"active_zero"` // seq=0
	ActiveDown  string `json:"active_down"` // open after dbdown
	Revoked     string `json:"revoked"`     // refused at open
	Unknown     string `json:"unknown"`     // no row
}

func main() {
	log.SetOutput(os.Stderr)
	adminDSN := os.Getenv("XLANG_DB_URL")
	migrations := os.Getenv("XLANG_MIGRATIONS_DIR")
	if adminDSN == "" || migrations == "" {
		log.Fatal("XLANG_DB_URL and XLANG_MIGRATIONS_DIR are required")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminDSN)
	must(err, "admin pool")
	defer admin.Close()
	dbName := "xlang_authz_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	_, err = admin.Exec(ctx, "CREATE DATABASE "+dbName)
	must(err, "create database")
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)"); err != nil {
			log.Printf("drop database: %v", err)
		}
	}()

	u, err := url.Parse(adminDSN)
	must(err, "parse dsn")
	u.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, u.String())
	must(err, "pool")
	must(applyMigrations(ctx, pool, migrations), "migrations")

	s := seed(ctx, pool)

	h := &connector.EnrollmentHandler{Pool: pool, Registry: connector.NewConnectorRegistry()}
	srv := grpc.NewServer(grpc.StreamInterceptor(identityFromHeader(s.TrustDomain)))
	pb.RegisterConnectorServiceServer(srv, h)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	must(err, "listen")
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	s.Addr = lis.Addr().String()
	line, _ := json.Marshal(s)
	fmt.Println(string(line))

	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		f := strings.Fields(in.Text())
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "revoke":
			if _, err := pool.Exec(ctx, `UPDATE connectors SET status='revoked', revoked_at=NOW(), updated_at=NOW() WHERE id=$1`, f[1]); err != nil {
				fmt.Printf("err revoke: %v\n", err)
				continue
			}
			n := h.Registry.SignalRevoked(f[1])
			fmt.Printf("ok revoke %d\n", n)
		case "delete":
			if _, err := pool.Exec(ctx, `DELETE FROM connectors WHERE id=$1`, f[1]); err != nil {
				fmt.Printf("err delete: %v\n", err)
				continue
			}
			fmt.Println("ok delete")
		case "dbdown":
			pool.Close()
			fmt.Println("ok dbdown")
		case "quit":
			fmt.Println("ok quit")
			return
		default:
			fmt.Printf("err %s: unknown command\n", f[0])
		}
	}
}

// identityFromHeader stands in for StreamSPIFFEInterceptor (which needs a
// client certificate): it injects the connector identity named by the
// x-xlang-connector-id header, exactly as the real interceptor would.
func identityFromHeader(trustDomain string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		md, _ := metadata.FromIncomingContext(ss.Context())
		id := ""
		if v := md.Get("x-xlang-connector-id"); len(v) > 0 {
			id = v[0]
		}
		ctx := spiffe.WithIdentity(ss.Context(),
			fmt.Sprintf("spiffe://%s/connector/%s", trustDomain, id), "connector", id, trustDomain)
		return handler(srv, &ctxStream{ServerStream: ss, ctx: ctx})
	}
}

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }

func seed(ctx context.Context, pool *pgxpool.Pool) seeded {
	slug := "xlang-" + uuid.NewString()[:8]
	td := "ws-" + slug + ".zecurity.in"
	var wsID, rnID string
	must(pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, trust_domain, status) VALUES ($1, $1, $2, 'active') RETURNING id`,
		slug, td).Scan(&wsID), "seed workspace")
	must(pool.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'rn', 'office') RETURNING id`,
		wsID).Scan(&rnID), "seed remote network")
	conn := func(status string, revoked bool) string {
		var id string
		q := `INSERT INTO connectors (tenant_id, remote_network_id, name, trust_domain, status, revoked_at)
		      VALUES ($1, $2, 'xlang', $3, $4, CASE WHEN $5 THEN NOW() ELSE NULL END) RETURNING id`
		must(pool.QueryRow(ctx, q, wsID, rnID, td, status, revoked).Scan(&id), "seed connector")
		return id
	}
	return seeded{
		TrustDomain: td,
		Active:      conn("active", false),
		ActiveDel:   conn("active", false),
		ActiveZero:  conn("active", false),
		ActiveDown:  conn("active", false),
		Revoked:     conn("revoked", true),
		Unknown:     uuid.NewString(),
	}
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("xlangharness: %s: %v", what, err)
	}
}
