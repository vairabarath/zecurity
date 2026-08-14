package outbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	DefaultPollInterval   = 1 * time.Second
	DefaultLockWindow     = 30 * time.Second
	DefaultReaperInterval = 30 * time.Second

	MinLockWindow     = 5 * time.Second
	MaxLockWindow     = 1 * time.Hour
	MinReaperInterval = 1 * time.Second
	MaxReaperInterval = 5 * time.Minute
)

type Processor struct {
	outbox         *Outbox
	registry       *HandlerRegistry
	pollInterval   time.Duration
	lockWindow     time.Duration
	reaperInterval time.Duration
}

type ProcessorOption func(*processorConfig)

type processorConfig struct {
	pollInterval   time.Duration
	lockWindow     time.Duration
	reaperInterval time.Duration
	maxRetries     int
}

func defaultProcessorConfig() processorConfig {
	return processorConfig{
		pollInterval:   DefaultPollInterval,
		lockWindow:     DefaultLockWindow,
		reaperInterval: DefaultReaperInterval,
		maxRetries:     DefaultMaxRetries,
	}
}

func WithPollInterval(interval time.Duration) ProcessorOption {
	return func(cfg *processorConfig) {
		cfg.pollInterval = interval
	}
}

func WithLockWindow(window time.Duration) ProcessorOption {
	return func(cfg *processorConfig) {
		cfg.lockWindow = window
	}
}

func WithReaperInterval(interval time.Duration) ProcessorOption {
	return func(cfg *processorConfig) {
		cfg.reaperInterval = interval
	}
}

func WithMaxRetries(maxRetries int) ProcessorOption {
	return func(cfg *processorConfig) {
		cfg.maxRetries = maxRetries
	}
}

func NewProcessor(
	o *Outbox,
	registry *HandlerRegistry,
	opts ...ProcessorOption,
) (*Processor, error) {
	if o == nil {
		return nil, fmt.Errorf("outbox processor requires an outbox")
	}

	if registry == nil {
		return nil, fmt.Errorf("outbox processor requires a handler registry")
	}

	cfg := defaultProcessorConfig()

	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	if cfg.pollInterval <= 0 {
		return nil, fmt.Errorf("outbox processor poll interval must be positive")
	}

	if cfg.lockWindow < MinLockWindow || cfg.lockWindow > MaxLockWindow {
		return nil, fmt.Errorf(
			"outbox processor lock window must be between %s and %s",
			MinLockWindow,
			MaxLockWindow,
		)
	}

	if cfg.reaperInterval < MinReaperInterval ||
		cfg.reaperInterval > MaxReaperInterval {
		return nil, fmt.Errorf(
			"outbox processor reaper interval must be between %s and %s",
			MinReaperInterval,
			MaxReaperInterval,
		)
	}

	if cfg.reaperInterval > cfg.lockWindow {
		return nil, fmt.Errorf(
			"outbox processor reaper interval %s must not exceed lock window %s",
			cfg.reaperInterval,
			cfg.lockWindow,
		)
	}

	if cfg.maxRetries < 1 || cfg.maxRetries > 1000 {
		return nil, fmt.Errorf(
			"outbox processor max retries must be between 1 and 1000: %d",
			cfg.maxRetries,
		)
	}

	if o.retryPolicy.MaxRetries != cfg.maxRetries {
		return nil, fmt.Errorf(
			"outbox processor max retries %d does not match outbox max retries %d",
			cfg.maxRetries,
			o.retryPolicy.MaxRetries,
		)
	}

	return &Processor{
		outbox:         o,
		registry:       registry,
		pollInterval:   cfg.pollInterval,
		lockWindow:     cfg.lockWindow,
		reaperInterval: cfg.reaperInterval,
	}, nil
}

func (p *Processor) processEvent(
	ctx context.Context,
	evt OutboxEvent,
) error {
	if evt.LeaseID == nil {
		return fmt.Errorf("event %s has no lease", evt.ID)
	}

	leaseID := *evt.LeaseID

	err := p.registry.Dispatch(ctx, evt)
	if err == nil {
		if err := p.outbox.MarkDone(ctx, evt.ID, leaseID); err != nil {
			return fmt.Errorf("mark event done: %w", err)
		}

		log.Printf(
			"outbox: completed event id=%s event_type=%s workspace_id=%s user_id=%v correlation_id=%s",
			evt.ID,
			evt.EventType,
			evt.WorkspaceID,
			evt.UserID,
			evt.CorrelationID,
		)

		return nil
	}

	if errors.Is(err, ErrNoHandler) {
		if markErr := p.outbox.MarkFailedTerminal(
			ctx,
			evt.ID,
			leaseID,
			err,
		); markErr != nil {
			return fmt.Errorf(
				"mark unknown handler terminal: %w",
				markErr,
			)
		}

		log.Printf(
			"outbox: terminal failure id=%s event_type=%s workspace_id=%s user_id=%v correlation_id=%s retry_count=%d error=%v",
			evt.ID,
			evt.EventType,
			evt.WorkspaceID,
			evt.UserID,
			evt.CorrelationID,
			p.outbox.retryPolicy.MaxRetries,
			err,
		)

		return err
	}

	if markErr := p.outbox.MarkFailed(
		ctx,
		evt.ID,
		leaseID,
		err,
	); markErr != nil {
		return fmt.Errorf(
			"mark event failed: %w (handler error: %v)",
			markErr,
			err,
		)
	}

	retryCount := evt.RetryCount + 1

	if retryCount >= p.outbox.retryPolicy.MaxRetries {
		log.Printf(
			"outbox: terminal failure id=%s event_type=%s workspace_id=%s user_id=%v correlation_id=%s retry_count=%d error=%v",
			evt.ID,
			evt.EventType,
			evt.WorkspaceID,
			evt.UserID,
			evt.CorrelationID,
			retryCount,
			err,
		)
	} else {
		log.Printf(
			"outbox: processing failure id=%s event_type=%s workspace_id=%s user_id=%v correlation_id=%s retry_count=%d error=%v",
			evt.ID,
			evt.EventType,
			evt.WorkspaceID,
			evt.UserID,
			evt.CorrelationID,
			retryCount,
			err,
		)
	}

	return err
}

// Run starts the background processor until the context is cancelled.
//
// The processor claims eligible events on pollInterval, dispatches each
// claimed event through the registry, and marks outcomes. A separate
// reaper goroutine resets expired leases at reaperInterval. Both loops
// exit on context cancellation and the processor returns after the
// goroutines stop.
func (p *Processor) Run(ctx context.Context, batchSize int) error {
	if ctx == nil {
		return fmt.Errorf("outbox processor requires a context")
	}

	if batchSize <= 0 {
		return fmt.Errorf("outbox processor batch size must be positive")
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(2)

	go func() {
		defer wg.Done()

		ticker := time.NewTicker(p.pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				events, err := p.outbox.ClaimEvents(ctx, batchSize)
				if err != nil {
					errCh <- fmt.Errorf("claim outbox events: %w", err)
					continue
				}

				for _, evt := range events {
					if err := p.processEvent(ctx, evt); err != nil {
						// Handler failures are expected and already persisted.
						// Do not terminate the processor.
					}
				}
			}
		}
	}()

	go func() {
		defer wg.Done()

		ticker := time.NewTicker(p.reaperInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				reaped, err := p.outbox.ReapAbandoned(ctx, p.lockWindow)
				if err != nil {
					errCh <- fmt.Errorf("reap abandoned outbox events: %w", err)
					continue
				}
				log.Printf("outbox: reaper recovered %d abandoned event(s)",
					reaped,
				)
			}
		}
	}()

	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}
