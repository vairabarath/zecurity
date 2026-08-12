package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEnqueueIntegration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := outboxTestDBName(t)

	adminPool := outboxTestPool(t, ctx, adminDSN)
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := outboxTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database DSN: %v", err)
	}

	pool := outboxTestPool(t, ctx, testDSN)
	defer pool.Close()

	if err := applyOutboxTestMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	workspaceID := insertOutboxTestWorkspace(t, ctx, pool)
	userID := insertOutboxTestUser(t, ctx, pool, workspaceID)

	store := NewOutbox(pool)

	correlationID := uuid.New()
	payload := json.RawMessage(`{"user_id":"test-user","action":"provision"}`)

	event := Event{
		EventType:     "identity.user.provisioned",
		WorkspaceID:   workspaceID,
		UserID:        &userID,
		CorrelationID: correlationID,
		Payload:       payload,
	}

	t.Run("commit persists event", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin transaction: %v", err)
		}
		defer tx.Rollback(ctx)

		if err := store.Enqueue(ctx, tx, event); err != nil {
			t.Fatalf("enqueue event: %v", err)
		}

		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit transaction: %v", err)
		}

		var (
			gotEventType     string
			gotWorkspaceID   uuid.UUID
			gotUserID        *uuid.UUID
			gotCorrelationID uuid.UUID
			gotPayload       []byte
			gotStatus        string
			gotRetryCount    int
			gotNextAttempt   *time.Time
		)

		err = pool.QueryRow(
			ctx,
			`SELECT
				event_type,
				workspace_id,
				user_id,
				correlation_id,
				payload,
				status,
				retry_count,
				next_attempt_at
			FROM outbox_events
			WHERE correlation_id = $1`,
			correlationID,
		).Scan(
			&gotEventType,
			&gotWorkspaceID,
			&gotUserID,
			&gotCorrelationID,
			&gotPayload,
			&gotStatus,
			&gotRetryCount,
			&gotNextAttempt,
		)
		if err != nil {
			t.Fatalf("load committed event: %v", err)
		}

		if gotEventType != event.EventType {
			t.Fatalf("event_type = %q, want %q", gotEventType, event.EventType)
		}

		if gotWorkspaceID != event.WorkspaceID {
			t.Fatalf("workspace_id = %s, want %s", gotWorkspaceID, event.WorkspaceID)
		}

		if gotUserID == nil || *gotUserID != event.UserIDValue() {
			t.Fatalf("user_id = %v, want %s", gotUserID, event.UserIDValue())
		}

		if gotCorrelationID != event.CorrelationID {
			t.Fatalf("correlation_id = %s, want %s", gotCorrelationID, event.CorrelationID)
		}

		if string(gotPayload) != string(event.Payload) {
			t.Fatalf("payload = %s, want %s", gotPayload, event.Payload)
		}

		if gotStatus != "pending" {
			t.Fatalf("status = %q, want %q", gotStatus, "pending")
		}

		if gotRetryCount != 0 {
			t.Fatalf("retry_count = %d, want 0", gotRetryCount)
		}

		if gotNextAttempt == nil {
			t.Fatal("next_attempt_at is NULL, want populated timestamp")
		}
	})

	t.Run("rollback removes event", func(t *testing.T) {
		rollbackCorrelationID := uuid.New()

		rollbackEvent := Event{
			EventType:     "identity.user.provisioned",
			WorkspaceID:   workspaceID,
			UserID:        &userID,
			CorrelationID: rollbackCorrelationID,
			Payload:       json.RawMessage(`{"rollback":true}`),
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin transaction: %v", err)
		}

		if err := store.Enqueue(ctx, tx, rollbackEvent); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("enqueue event: %v", err)
		}

		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback transaction: %v", err)
		}

		var count int
		if err := pool.QueryRow(
			ctx,
			`SELECT COUNT(*)
			 FROM outbox_events
			 WHERE correlation_id = $1`,
			rollbackCorrelationID,
		).Scan(&count); err != nil {
			t.Fatalf("count rolled-back events: %v", err)
		}

		if count != 0 {
			t.Fatalf("rolled-back event count = %d, want 0", count)
		}
	})

	t.Run("nil transaction is rejected", func(t *testing.T) {
		err := store.Enqueue(ctx, nil, Event{
			EventType:     "identity.user.provisioned",
			WorkspaceID:   workspaceID,
			UserID:        &userID,
			CorrelationID: uuid.New(),
			Payload:       json.RawMessage(`{"nil_tx":true}`),
		})

		if !errors.Is(err, ErrNilTx) {
			t.Fatalf("error = %v, want %v", err, ErrNilTx)
		}
	})
}

func (e Event) UserIDValue() uuid.UUID {
	if e.UserID == nil {
		return uuid.Nil
	}
	return *e.UserID
}

func outboxTestDBName(t *testing.T) string {
	t.Helper()

	name := strings.ToLower(t.Name())
	name = strings.NewReplacer("/", "_", " ", "_", "-", "_").Replace(name)

	return fmt.Sprintf(
		"outbox_%s_%d_%d",
		name,
		os.Getpid(),
		time.Now().UnixNano(),
	)
}

func outboxTestDSN(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}

	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func outboxTestPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("create postgres pool: %v", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping postgres: %v", err)
	}

	return pool
}

func applyOutboxTestMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		return fmt.Errorf("resolve migrations: %w", err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}

	sort.Strings(files)

	for _, filename := range files {
		contents, err := os.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read %s: %w", filename, err)
		}

		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			return fmt.Errorf("execute %s: %w", filepath.Base(filename), err)
		}
	}

	return nil
}

func insertOutboxTestWorkspace(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	slug := fmt.Sprintf("outbox-%d", time.Now().UnixNano())

	err := pool.QueryRow(
		ctx,
		`INSERT INTO workspaces (
			slug,
			name,
			status,
			trust_domain
		)
		VALUES ($1, $1, 'active', $1 || '.test')
		RETURNING id`,
		slug,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	return id
}

func insertOutboxTestUser(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	workspaceID uuid.UUID,
) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	email := fmt.Sprintf("outbox-%d@example.test", time.Now().UnixNano())

	err := pool.QueryRow(
		ctx,
		`INSERT INTO users (
			email
		)
		VALUES ($1)
		RETURNING id`,
		email,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// Keep the user associated with the workspace using the repository's
	// existing workspace_members relationship.
	_, err = pool.Exec(
		ctx,
		`INSERT INTO workspace_members (
			workspace_id,
			user_id
		)
		VALUES ($1, $2)`,
		workspaceID,
		id,
	)
	if err != nil {
		t.Fatalf("insert workspace membership: %v", err)
	}

	return id
}

// Ensure pgx remains referenced by this integration test package when the
// repository's pgx version changes its concrete transaction interfaces.
var _ pgx.Tx
