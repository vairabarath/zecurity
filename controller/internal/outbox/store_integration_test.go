package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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

		var gotJSON any
		var wantJSON any

		if err := json.Unmarshal(gotPayload, &gotJSON); err != nil {
			t.Fatalf("decode stored payload: %v", err)
		}

		if err := json.Unmarshal(event.Payload, &wantJSON); err != nil {
			t.Fatalf("decode expected payload: %v", err)
		}

		if !reflect.DeepEqual(gotJSON, wantJSON) {
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

	email := fmt.Sprintf(
		"outbox-test-%d@example.test",
		time.Now().UnixNano(),
	)

	providerSub := fmt.Sprintf(
		"outbox-test-%d",
		time.Now().UnixNano(),
	)

	if err := pool.QueryRow(
		ctx,
		`INSERT INTO users (
			tenant_id,
			email,
			provider,
			provider_sub,
			role,
			status
		)
		VALUES ($1, $2, 'test', $3, 'member', 'active')
		RETURNING id`,
		workspaceID,
		email,
		providerSub,
	).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	return id
}

// Ensure pgx remains referenced by this integration test package when the
// repository's pgx version changes its concrete transaction interfaces.
var _ pgx.Tx

func TestClaimEventsIntegration(t *testing.T) {
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

	events := []Event{
		{
			EventType:     "test.claim.one",
			WorkspaceID:   workspaceID,
			UserID:        &userID,
			CorrelationID: uuid.New(),
			Payload:       json.RawMessage(`{"n":1}`),
		},
		{
			EventType:     "test.claim.two",
			WorkspaceID:   workspaceID,
			UserID:        &userID,
			CorrelationID: uuid.New(),
			Payload:       json.RawMessage(`{"n":2}`),
		},
	}

	for _, event := range events {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin enqueue transaction: %v", err)
		}

		if err := store.Enqueue(ctx, tx, event); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("enqueue event: %v", err)
		}

		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit enqueue transaction: %v", err)
		}
	}

	claimed, err := store.ClaimEvents(ctx, 1)
	if err != nil {
		t.Fatalf("claim events: %v", err)
	}

	if len(claimed) != 1 {
		t.Fatalf("claimed %d events, want 1", len(claimed))
	}

	event := claimed[0]

	if event.Status != "processing" {
		t.Fatalf("status = %q, want processing", event.Status)
	}

	if event.LeaseID == nil {
		t.Fatal("lease_id is nil, want generated lease")
	}

	if event.ClaimedAt == nil {
		t.Fatal("claimed_at is nil, want populated timestamp")
	}

	if event.UpdatedAt.IsZero() {
		t.Fatal("updated_at is zero")
	}

	if event.EventType != "test.claim.one" {
		t.Fatalf("event_type = %q, want test.claim.one", event.EventType)
	}

	var remaining int
	if err := pool.QueryRow(
		ctx,
		`SELECT COUNT(*)
		   FROM outbox_events
		  WHERE status = 'pending'`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count pending events: %v", err)
	}

	if remaining != 1 {
		t.Fatalf("pending events = %d, want 1", remaining)
	}
}

func TestMarkDoneIntegration(t *testing.T) {
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

	event := Event{
		EventType:     "test.done",
		WorkspaceID:   workspaceID,
		UserID:        &userID,
		CorrelationID: uuid.New(),
		Payload:       json.RawMessage(`{"done":true}`),
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}

	if err := store.Enqueue(ctx, tx, event); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("enqueue: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	claimed, err := store.ClaimEvents(ctx, 1)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if len(claimed) != 1 {
		t.Fatalf("claimed %d events, want 1", len(claimed))
	}

	claimedEvent := claimed[0]

	if claimedEvent.LeaseID == nil {
		t.Fatal("claimed event has nil lease_id")
	}

	if err := store.MarkDone(ctx, claimedEvent.ID, *claimedEvent.LeaseID); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	var status string
	if err := pool.QueryRow(
		ctx,
		`SELECT status FROM outbox_events WHERE id = $1`,
		claimedEvent.ID,
	).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}

	if status != "done" {
		t.Fatalf("status = %q, want done", status)
	}
}

func TestStaleLeaseIntegration(t *testing.T) {
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

	event := Event{
		EventType:     "test.stale",
		WorkspaceID:   workspaceID,
		UserID:        &userID,
		CorrelationID: uuid.New(),
		Payload:       json.RawMessage(`{"stale":true}`),
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	if err := store.Enqueue(ctx, tx, event); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("enqueue: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	claimed, err := store.ClaimEvents(ctx, 1)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if len(claimed) != 1 {
		t.Fatalf("claimed %d events, want 1", len(claimed))
	}

	claimedEvent := claimed[0]

	if claimedEvent.LeaseID == nil {
		t.Fatal("lease_id is nil")
	}

	staleLease := uuid.New()

	if err := store.MarkDone(ctx, claimedEvent.ID, staleLease); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("MarkDone stale lease error = %v, want %v", err, ErrStaleLease)
	}

	if err := store.MarkFailed(ctx, claimedEvent.ID, staleLease, errors.New("stale worker")); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("MarkFailed stale lease error = %v, want %v", err, ErrStaleLease)
	}
}

func TestConcurrentClaimEventsIntegration(t *testing.T) {
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

	const eventCount = 20

	for i := 0; i < eventCount; i++ {
		event := Event{
			EventType:     "test.concurrent",
			WorkspaceID:   workspaceID,
			UserID:        &userID,
			CorrelationID: uuid.New(),
			Payload:       json.RawMessage(fmt.Sprintf(`{"index":%d}`, i)),
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin enqueue: %v", err)
		}

		if err := store.Enqueue(ctx, tx, event); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("enqueue event %d: %v", i, err)
		}

		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit event %d: %v", i, err)
		}
	}

	type result struct {
		events []OutboxEvent
		err    error
	}

	results := make(chan result, 2)

	go func() {
		events, err := store.ClaimEvents(ctx, eventCount)
		results <- result{events: events, err: err}
	}()

	go func() {
		events, err := store.ClaimEvents(ctx, eventCount)
		results <- result{events: events, err: err}
	}()

	first := <-results
	second := <-results

	if first.err != nil {
		t.Fatalf("first worker claim: %v", first.err)
	}

	if second.err != nil {
		t.Fatalf("second worker claim: %v", second.err)
	}

	seen := make(map[uuid.UUID]struct{}, eventCount)

	for _, event := range first.events {
		if _, exists := seen[event.ID]; exists {
			t.Fatalf("duplicate event %s claimed by first worker", event.ID)
		}
		seen[event.ID] = struct{}{}
	}

	for _, event := range second.events {
		if _, exists := seen[event.ID]; exists {
			t.Fatalf("event %s claimed by both workers", event.ID)
		}
		seen[event.ID] = struct{}{}
	}

	if len(seen) != eventCount {
		t.Fatalf(
			"unique claimed events = %d, want %d",
			len(seen),
			eventCount,
		)
	}

	var processingCount int
	if err := pool.QueryRow(
		ctx,
		`SELECT COUNT(*)
		   FROM outbox_events
		  WHERE status = 'processing'`,
	).Scan(&processingCount); err != nil {
		t.Fatalf("count processing events: %v", err)
	}

	if processingCount != eventCount {
		t.Fatalf(
			"processing events = %d, want %d",
			processingCount,
			eventCount,
		)
	}
}

func TestMarkFailedIntegration(t *testing.T) {
	// Reuse the same DB setup pattern as TestMarkDoneIntegration.

	// create workspace/user
	// enqueue event
	// claim event

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

	event := Event{
		EventType:     "test.done",
		WorkspaceID:   workspaceID,
		UserID:        &userID,
		CorrelationID: uuid.New(),
		Payload:       json.RawMessage(`{"done":true}`),
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}

	if err := store.Enqueue(ctx, tx, event); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("enqueue: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	claimed, err := store.ClaimEvents(ctx, 1)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if len(claimed) != 1 {
		t.Fatalf("claimed %d events, want 1", len(claimed))
	}

	claimedEvent := claimed[0]

	if claimedEvent.LeaseID == nil {
		t.Fatal("claimed event has nil lease_id")
	}

	handlerErr := errors.New("handler failed")

	if err := store.MarkFailed(
		ctx,
		claimedEvent.ID,
		*claimedEvent.LeaseID,
		handlerErr,
	); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	var (
		status        string
		retryCount    int
		lastError     *string
		leaseID       *uuid.UUID
		claimedAt     *time.Time
		nextAttemptAt *time.Time
	)

	err = pool.QueryRow(
		ctx,
		`SELECT status,
		        retry_count,
		        last_error,
		        lease_id,
		        claimed_at,
				next_attempt_at
		   FROM outbox_events
		  WHERE id = $1`,
		claimedEvent.ID,
	).Scan(
		&status,
		&retryCount,
		&lastError,
		&leaseID,
		&claimedAt,
		&nextAttemptAt,
	)
	if err != nil {
		t.Fatalf("load failed event: %v", err)
	}

	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}

	if retryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", retryCount)
	}

	if nextAttemptAt == nil {
		t.Fatal("next_attempt_at = NULL, want a scheduled retry")
	}

	if !nextAttemptAt.After(time.Now().UTC()) {
		t.Fatalf(
			"next_attempt_at = %v, want a future retry time",
			*nextAttemptAt,
		)
	}
	if lastError == nil || *lastError != handlerErr.Error() {
		t.Fatalf("last_error = %v, want %q", lastError, handlerErr.Error())
	}

	if leaseID != nil {
		t.Fatalf("lease_id = %v, want NULL", leaseID)
	}

	if claimedAt != nil {
		t.Fatalf("claimed_at = %v, want NULL", claimedAt)
	}
}

func TestMaxRetriesIntegration(t *testing.T) {
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

	store, err := NewOutboxWithMaxRetries(pool, 2)
	if err != nil {
		t.Fatalf("create outbox store: %v", err)
	}

	event := Event{
		EventType:     "test.retry.exhaustion",
		WorkspaceID:   workspaceID,
		UserID:        &userID,
		CorrelationID: uuid.New(),
		Payload:       json.RawMessage(`{"retry":true}`),
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}

	if err := store.Enqueue(ctx, tx, event); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("enqueue: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	claimed, err := store.ClaimEvents(ctx, 1)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	if len(claimed) != 1 {
		t.Fatalf("first claimed events = %d, want 1", len(claimed))
	}

	first := claimed[0]
	if first.LeaseID == nil {
		t.Fatal("first claimed event has nil lease_id")
	}

	if err := store.MarkFailed(
		ctx,
		first.ID,
		*first.LeaseID,
		errors.New("first failure"),
	); err != nil {
		t.Fatalf("first mark failed: %v", err)
	}

	var (
		firstStatus      string
		firstRetryCount  int
		firstNextAttempt *time.Time
		firstLastError   *string
	)

	err = pool.QueryRow(
		ctx,
		`SELECT status,
		        retry_count,
		        next_attempt_at,
		        last_error
		   FROM outbox_events
		  WHERE id = $1`,
		first.ID,
	).Scan(
		&firstStatus,
		&firstRetryCount,
		&firstNextAttempt,
		&firstLastError,
	)
	if err != nil {
		t.Fatalf("load first failed event: %v", err)
	}

	if firstStatus != "failed" {
		t.Fatalf("first status = %q, want failed", firstStatus)
	}

	if firstRetryCount != 1 {
		t.Fatalf("first retry_count = %d, want 1", firstRetryCount)
	}

	if firstNextAttempt == nil {
		t.Fatal("first next_attempt_at = NULL, want scheduled retry")
	}

	if firstLastError == nil || *firstLastError != "first failure" {
		t.Fatalf("first last_error = %v, want %q", firstLastError, "first failure")
	}

	_, err = pool.Exec(
		ctx,
		`UPDATE outbox_events
		    SET next_attempt_at = NOW()
		  WHERE id = $1`,
		first.ID,
	)
	if err != nil {
		t.Fatalf("make retry eligible: %v", err)
	}

	claimed, err = store.ClaimEvents(ctx, 1)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}

	if len(claimed) != 1 {
		t.Fatalf("second claimed events = %d, want 1", len(claimed))
	}

	second := claimed[0]
	if second.LeaseID == nil {
		t.Fatal("second claimed event has nil lease_id")
	}

	if err := store.MarkFailed(
		ctx,
		second.ID,
		*second.LeaseID,
		errors.New("final failure"),
	); err != nil {
		t.Fatalf("second mark failed: %v", err)
	}

	var (
		secondStatus      string
		secondRetryCount  int
		secondNextAttempt *time.Time
		secondLastError   *string
	)

	err = pool.QueryRow(
		ctx,
		`SELECT status,
		        retry_count,
		        next_attempt_at,
		        last_error
		   FROM outbox_events
		  WHERE id = $1`,
		second.ID,
	).Scan(
		&secondStatus,
		&secondRetryCount,
		&secondNextAttempt,
		&secondLastError,
	)
	if err != nil {
		t.Fatalf("load exhausted event: %v", err)
	}

	if secondStatus != "failed" {
		t.Fatalf("status = %q, want failed", secondStatus)
	}

	if secondRetryCount != 2 {
		t.Fatalf("retry_count = %d, want 2", secondRetryCount)
	}

	if secondNextAttempt != nil {
		t.Fatalf(
			"next_attempt_at = %v, want NULL because retries are exhausted",
			*secondNextAttempt,
		)
	}

	if secondLastError == nil || *secondLastError != "final failure" {
		t.Fatalf(
			"last_error = %v, want %q",
			secondLastError,
			"final failure",
		)
	}
}
