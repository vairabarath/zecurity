package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/db"
)

// Audit chain verifier (PENDING-11 S1b):
//
//	zecurity-controller audit verify --workspace <workspace id> [--stream admin|access]
//
// Run with the controller's DATABASE_URL. It recomputes each stream's hash
// chain in Go from the raw columns (audit/canonical.go), from the retention
// checkpoint to the head, and reports missing, extra or edited rows. It only
// reads. Exit codes: 0 intact, 3 broken, 1 error, 2 usage.

func isAuditVerifyCommand(args []string) bool {
	return len(args) >= 3 && args[1] == "audit" && args[2] == "verify"
}

type auditVerifyArgs struct {
	workspace string
	streams   []audit.Stream
}

func parseAuditVerifyArgs(args []string, stderr io.Writer) (auditVerifyArgs, error) {
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ws := fs.String("workspace", "", "workspace id")
	stream := fs.String("stream", "", "admin or access (default: both)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: zecurity-controller audit verify --workspace <workspace id> [--stream admin|access]")
	}
	if err := fs.Parse(args); err != nil {
		return auditVerifyArgs{}, err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return auditVerifyArgs{}, errors.New("audit verify: unexpected arguments")
	}
	id, err := uuid.Parse(*ws)
	if err != nil {
		fs.Usage()
		return auditVerifyArgs{}, errors.New("audit verify: --workspace must be a workspace id")
	}
	out := auditVerifyArgs{workspace: id.String()}
	switch *stream {
	case "":
		out.streams = []audit.Stream{audit.StreamAdmin, audit.StreamAccess}
	case string(audit.StreamAdmin), string(audit.StreamAccess):
		out.streams = []audit.Stream{audit.Stream(*stream)}
	default:
		fs.Usage()
		return auditVerifyArgs{}, errors.New("audit verify: --stream must be admin or access")
	}
	return out, nil
}

// runAuditVerify returns the process exit code.
func runAuditVerify(args []string) int {
	a, err := parseAuditVerifyArgs(args, os.Stderr)
	if err != nil {
		return 2
	}
	loadOptionalEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := db.Init(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "audit verify: database: %v\n", err)
		return 1
	}
	defer db.Close()
	// One REPEATABLE READ snapshot: a retention purge landing between the
	// head read and the row pages would otherwise read as missing rows.
	code := 1
	err = pgx.BeginTxFunc(ctx, db.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			code = verifyStreams(ctx, tx, a, os.Stdout, os.Stderr)
			return nil
		})
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit verify: %v\n", err)
		return 1
	}
	return code
}

func verifyStreams(ctx context.Context, q audit.Querier, a auditVerifyArgs, stdout, stderr io.Writer) int {
	code := 0
	for _, s := range a.streams {
		res, err := audit.VerifyStream(ctx, q, a.workspace, s)
		if err != nil {
			fmt.Fprintf(stderr, "audit verify: %s: %v\n", s, err)
			return 1
		}
		status := "OK"
		if !res.OK() {
			status = "BROKEN"
			code = 3
		}
		fmt.Fprintf(stdout, "%s stream: %s (seq %d..%d, %d rows checked)\n", s, status, res.FromSeq, res.HeadSeq, res.RowsChecked)
		for _, b := range res.Breaks {
			fmt.Fprintf(stdout, "  seq %d: %s\n", b.Seq, b.Reason)
		}
	}
	return code
}
