package alerting

import (
	"context"
	"log"
	"time"
)

// Notifier receives the events of each evaluation after they are committed.
// A2 (webhook and email delivery, D-52) implements it; until then
// LogNotifier records them in the controller log. Notify must not block the
// evaluator for long (A2 queues deliveries through the outbox).
type Notifier interface {
	Notify(ctx context.Context, events []Event)
}

// LogNotifier logs one line per event (no tenant content beyond the target
// name and summary the provider already sees).
type LogNotifier struct{}

// Notify implements Notifier.
func (LogNotifier) Notify(_ context.Context, events []Event) {
	for _, e := range events {
		log.Printf("alert %s: %s [%s/%s] %s", e.Transition, e.Kind, e.Severity, e.Scope, e.Summary)
	}
}

// RuleSource is what the evaluator needs from the store.
type RuleSource interface {
	ListRules(ctx context.Context) ([]Rule, error)
	Apply(ctx context.Context, r Rule, obs []Observation, now time.Time, resolvedSummary string) ([]Event, error)
}

// Observer evaluates a rule's condition (Sources in production).
type Observer interface {
	Observe(ctx context.Context, kind string, p Params, now time.Time) ([]Observation, error)
}

// Evaluator runs every rule on an interval (D-02: one process-local loop).
type Evaluator struct {
	Store    RuleSource
	Observer Observer
	Notifier Notifier
	Now      func() time.Time
	// RuleTimeout bounds one rule's read (default 20 s).
	RuleTimeout time.Duration
}

// DefaultInterval is how often the evaluator runs.
const DefaultInterval = 30 * time.Second

// Run evaluates every interval until ctx ends.
func (e *Evaluator) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	log.Printf("alert evaluator: every %s", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		e.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick evaluates every rule once and returns the events recorded.
//
// Fail safe (Sprint 24 rules): when a rule's condition can't be read, its
// states are left exactly as they are (no false "resolved"), and the error
// is logged. A disabled rule is applied with no observations, which
// resolves whatever it has firing.
func (e *Evaluator) Tick(ctx context.Context) []Event {
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	timeout := e.RuleTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	rules, err := e.Store.ListRules(ctx)
	if err != nil {
		log.Printf("alert evaluator: list rules: %v", err)
		return nil
	}
	var all []Event
	for _, r := range rules {
		if ctx.Err() != nil {
			break
		}
		var obs []Observation
		if r.Enabled {
			rctx, cancel := context.WithTimeout(ctx, timeout)
			obs, err = e.Observer.Observe(rctx, r.Kind, r.Params, now())
			cancel()
			if err != nil {
				log.Printf("alert evaluator: rule %q (%s) not evaluated, alerts unchanged: %v", r.Name, r.Kind, err)
				continue
			}
		}
		events, err := e.Store.Apply(ctx, r, obs, now(), "")
		if err != nil {
			log.Printf("alert evaluator: rule %q (%s): apply: %v", r.Name, r.Kind, err)
			continue
		}
		all = append(all, events...)
	}
	if len(all) > 0 && e.Notifier != nil {
		e.Notifier.Notify(ctx, all)
	}
	return all
}
