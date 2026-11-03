package posture

import (
	"context"
	"time"

	
)

const (
    DefaultRetention = 30 * 24 * time.Hour
    DefaultBatchSize = 2000
)
type RetentionWorker struct {
    store     *Store
    retention time.Duration
    batchSize int
	now		func() time.Time
}

func NewRetentionWorker(
	store *Store,
	retention time.Duration,
	batchSize int,
	now func() time.Time,
) *RetentionWorker {

	if retention <= 0{
		retention = DefaultRetention
	}

	if batchSize <= 0{
		batchSize = DefaultBatchSize
	}
	if now == nil{
		now = time.Now
	}
	return &RetentionWorker{
		store: store,
		retention: retention,
		batchSize: batchSize,
		now: now,
	}
}

func (w *RetentionWorker) Run(ctx context.Context) {
	// Run once immediately.
	_ = w.cleanup(ctx)

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			_ = w.cleanup(ctx)
		}
	}
}

func (w *RetentionWorker) cleanup(ctx context.Context) error {
	cutoff := w.now().Add(-w.retention)

	for {
		deleted, err := w.store.DeleteExpiredReports(ctx, cutoff, w.batchSize)
		if err != nil {
			return err
		}

		if deleted < int64(w.batchSize) {
			return nil
		}
	}
}