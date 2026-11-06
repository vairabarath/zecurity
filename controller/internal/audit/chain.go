package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Stream names one hash-chained audit stream of a workspace (PENDING-11 P-1).
// The chain itself is maintained by the database (migration 051): a BEFORE
// INSERT trigger assigns each row a gap-free, commit-ordered seq and
// row_hash = sha256(prev_hash || canonical(row)).
type Stream string

const (
	StreamAdmin  Stream = "admin"  // audit_logs
	StreamAccess Stream = "access" // connector_logs
)

// Valid reports whether s is a known stream.
func (s Stream) Valid() bool { return s == StreamAdmin || s == StreamAccess }

// Genesis is the prev_hash of a stream's first row:
// sha256("zecurity-audit-v1:" + workspace_id + ":" + stream).
func Genesis(workspaceID string, s Stream) []byte {
	sum := sha256.Sum256([]byte("zecurity-audit-v1:" + workspaceID + ":" + string(s)))
	return sum[:]
}

// RowHash is sha256(prev || canonical).
func RowHash(prev, canonical []byte) []byte {
	h := sha256.New()
	h.Write(prev)
	h.Write(canonical)
	return h.Sum(nil)
}

// Querier is the read side of a pgx pool, connection or transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Break is one place where a stream fails verification.
type Break struct {
	Seq    int64
	Reason string
}

// VerifyResult summarises a stream verification.
type VerifyResult struct {
	Stream      Stream
	FromSeq     int64 // first seq checked (purged_through_seq + 1)
	HeadSeq     int64 // the head's last_seq
	RowsChecked int64
	Breaks      []Break
}

// OK reports whether the stream verified cleanly.
func (r VerifyResult) OK() bool { return len(r.Breaks) == 0 }

// maxBreaks bounds the report; one break is enough to fail verification.
const maxBreaks = 100

// VerifyStream recomputes a workspace stream's hash chain in Go, from the
// retention checkpoint (or genesis) up to the head. It reports missing or
// extra seqs, prev_hash links that don't match, row hashes that don't match
// the row's current content, and a head that disagrees with the last row.
// The canonical bytes are built in Go from the raw columns (canonical.go),
// not by the database's audit_*_canonical functions, so a pass doesn't
// depend on trusting the database's own hashing code.
func VerifyStream(ctx context.Context, db Querier, workspaceID string, s Stream) (VerifyResult, error) {
	res := VerifyResult{Stream: s}
	if !s.Valid() {
		return res, fmt.Errorf("audit: unknown stream %q", s)
	}
	// Rows appended after the head is read are outside this check (seq bound).
	head, err := ReadHead(ctx, db, workspaceID, s)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("audit: no %s stream head for workspace %s", s, workspaceID)
	}
	if err != nil {
		return res, fmt.Errorf("audit: read stream head: %w", err)
	}
	res.HeadSeq = head.LastSeq

	prev := head.PurgedThroughHash
	if prev == nil {
		prev = Genesis(workspaceID, s)
	}
	res.FromSeq = head.PurgedThroughSeq + 1

	addBreak := func(seq int64, format string, args ...any) {
		if len(res.Breaks) < maxBreaks {
			res.Breaks = append(res.Breaks, Break{Seq: seq, Reason: fmt.Sprintf(format, args...)})
		}
	}
	check := func(seq int64, prevHash, rowHash, canonical []byte, expected int64) {
		res.RowsChecked++
		if seq != expected {
			addBreak(seq, "expected seq %d, found %d (rows missing)", expected, seq)
		}
		if !bytes.Equal(prevHash, prev) {
			addBreak(seq, "prev_hash does not match the previous row's hash")
		}
		if !bytes.Equal(rowHash, RowHash(prevHash, canonical)) {
			addBreak(seq, "row_hash does not match the row's content (row edited)")
		}
		prev = rowHash
	}

	const page = 1000
	expected := res.FromSeq
	after := head.PurgedThroughSeq
	for {
		n := 0
		switch s {
		case StreamAdmin:
			rows, err := ReadAdminRows(ctx, db, workspaceID, after, head.LastSeq, page)
			if err != nil {
				return res, err
			}
			for _, r := range rows {
				check(r.Seq, r.PrevHash, r.RowHash, r.Canonical(), expected)
				expected, after = r.Seq+1, r.Seq
			}
			n = len(rows)
		case StreamAccess:
			rows, err := ReadAccessRows(ctx, db, workspaceID, after, head.LastSeq, page)
			if err != nil {
				return res, err
			}
			for _, r := range rows {
				check(r.Seq, r.PrevHash, r.RowHash, r.Canonical(), expected)
				expected, after = r.Seq+1, r.Seq
			}
			n = len(rows)
		}
		if n < page {
			break
		}
	}

	if expected-1 != res.HeadSeq {
		addBreak(expected-1, "stream ends at seq %d but the head is at %d", expected-1, res.HeadSeq)
	} else if !bytes.Equal(prev, head.LastHash) {
		addBreak(res.HeadSeq, "head hash does not match the last row's hash")
	}
	return res, nil
}

// CheckAppendOnly reports whether the database role the controller runs as
// can change or remove audit rows (D-12). It returns the offending privileges,
// for example "UPDATE audit_logs"; empty means append-only holds. A non-empty
// result means DATABASE_URL still points at the schema owner instead of
// ztna_app.
func CheckAppendOnly(ctx context.Context, db Querier) ([]string, error) {
	var bad []string
	for _, table := range []string{"audit_logs", "connector_logs", "provider_audit_logs"} {
		for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
			var has bool
			if err := db.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, table, priv).Scan(&has); err != nil {
				return nil, fmt.Errorf("audit: check %s on %s: %w", priv, table, err)
			}
			if has {
				bad = append(bad, priv+" "+table)
			}
		}
	}
	var delWorkspaces bool
	if err := db.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'workspaces', 'DELETE')`).Scan(&delWorkspaces); err != nil {
		return nil, fmt.Errorf("audit: check DELETE on workspaces: %w", err)
	}
	if delWorkspaces {
		bad = append(bad, "DELETE workspaces (cascades through audit streams)")
	}
	return bad, nil
}
