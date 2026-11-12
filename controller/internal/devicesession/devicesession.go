// Package devicesession ends a client device's live tunnels when the login
// session it was enrolled under ends (PENDING-09 C-1, design D-1 … D-5).
//
// Every client device records its session (client_devices.session_id) and
// that session's deadlines (session_idle_until, session_max_until). This
// package:
//
//   - moves the idle deadline forward on every refresh of the session;
//   - revokes the device at once when the session ends explicitly
//     (sign-out, max lifetime refused at refresh) or when every session of
//     the user is ended server-side (generation bump, lifecycle check);
//   - revokes devices whose deadline has passed, in a sweep every
//     SweepInterval (sessions that simply idle or age out send no event).
//
// Deadlines are read from Postgres, never from Valkey: a device is not
// revoked because a Valkey key is missing (a flush or restart must not revoke
// every device, D-2).
//
// Revocation is the existing terminal path (D-5): revoked_at = NOW(), which
// the certificate-history trigger puts on the CRL and the ACL compiler drops,
// then one audit row per device and a policy notify per workspace, which
// pushes the new ACL to connectors so they cut the device's live sessions.
package devicesession

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/audit"
)

const (
	// SweepInterval is how often expired session deadlines are swept (D-4).
	SweepInterval = 60 * time.Second
	// sweepBatch caps the devices revoked per sweep query; a sweep repeats
	// until a batch comes back short.
	sweepBatch = 500
)

// Reasons recorded in the audit row.
const (
	ReasonSignedOut       = "signed_out"
	ReasonSessionExpired  = "session_expired"
	ReasonSessionIdle     = "session_idle"
	ReasonSessionsRevoked = "sessions_revoked"
)

// Notifier is the policy notifier (policy.Notifier satisfies it).
type Notifier interface {
	NotifyPolicyChange(ctx context.Context, workspaceID string) error
}

// Manager implements auth.SessionObserver and runs the expiry sweep.
type Manager struct {
	pool     *pgxpool.Pool
	audit    func(context.Context, audit.Entry)
	notifier atomic.Pointer[notifierBox]
	now      func() time.Time
}

type notifierBox struct{ n Notifier }

// New returns a Manager. audit may be nil. The policy notifier is set later
// with SetNotifier (it is built after the auth service in main).
func New(pool *pgxpool.Pool, rec func(context.Context, audit.Entry)) *Manager {
	return &Manager{pool: pool, audit: rec, now: time.Now}
}

// SetNotifier installs the policy notifier. Call before serving.
func (m *Manager) SetNotifier(n Notifier) {
	m.notifier.Store(&notifierBox{n: n})
}

// revoked is one device revoked by a statement.
type revoked struct {
	deviceID    string
	workspaceID string
	userID      string
}

// SessionRefreshed moves the idle deadline of the session's active devices.
func (m *Manager) SessionRefreshed(ctx context.Context, userID, sid string, idleUntil time.Time) {
	if _, err := m.pool.Exec(ctx,
		`UPDATE client_devices
		    SET session_idle_until = $3
		  WHERE user_id = $1 AND session_id = $2 AND revoked_at IS NULL`,
		userID, sid, idleUntil); err != nil {
		log.Printf("devicesession: move idle deadline (user %s): %v", userID, err)
	}
}

// SessionEnded revokes the active devices enrolled under (userID, sid).
func (m *Manager) SessionEnded(ctx context.Context, userID, sid, reason string) {
	if sid == "" {
		return
	}
	devs, err := m.revoke(ctx,
		`UPDATE client_devices SET revoked_at = NOW()
		  WHERE user_id = $1 AND session_id = $2 AND revoked_at IS NULL
		  RETURNING id, workspace_id, user_id`,
		userID, sid)
	if err != nil {
		log.Printf("devicesession: revoke devices of ended session (user %s): %v", userID, err)
		return
	}
	m.after(ctx, devs, userID, reason)
}

// UserSessionsEnded revokes every active device of the user.
func (m *Manager) UserSessionsEnded(ctx context.Context, userID, reason string) {
	devs, err := m.revoke(ctx,
		`UPDATE client_devices SET revoked_at = NOW()
		  WHERE user_id = $1 AND revoked_at IS NULL
		  RETURNING id, workspace_id, user_id`,
		userID)
	if err != nil {
		log.Printf("devicesession: revoke devices of user %s: %v", userID, err)
		return
	}
	m.after(ctx, devs, "", reason)
}

// Sweep revokes active devices whose session deadline has passed and returns
// how many it revoked. The idle and max deadlines are swept separately so
// each audit row names the right reason.
func (m *Manager) Sweep(ctx context.Context) (int, error) {
	total := 0
	for _, q := range []struct{ column, reason string }{
		{"session_max_until", ReasonSessionExpired},
		{"session_idle_until", ReasonSessionIdle},
	} {
		for {
			devs, err := m.revoke(ctx,
				`UPDATE client_devices SET revoked_at = NOW()
				  WHERE id IN (
				        SELECT id FROM client_devices
				         WHERE revoked_at IS NULL AND session_id IS NOT NULL
				           AND `+q.column+` < $1
				         ORDER BY `+q.column+`
				         LIMIT $2
				         FOR UPDATE SKIP LOCKED)
				    AND revoked_at IS NULL
				  RETURNING id, workspace_id, user_id`,
				m.now(), sweepBatch)
			if err != nil {
				return total, err
			}
			m.after(ctx, devs, "", q.reason)
			total += len(devs)
			if len(devs) < sweepBatch {
				break
			}
		}
	}
	return total, nil
}

// Run sweeps every SweepInterval until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		if n, err := m.Sweep(ctx); err != nil {
			if ctx.Err() == nil {
				log.Printf("devicesession: sweep: %v", err)
			}
		} else if n > 0 {
			log.Printf("devicesession: revoked %d device(s) whose session ended", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) revoke(ctx context.Context, sql string, args ...any) ([]revoked, error) {
	rows, err := m.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []revoked
	for rows.Next() {
		var r revoked
		if err := rows.Scan(&r.deviceID, &r.workspaceID, &r.userID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// after writes one audit row per revoked device and notifies each workspace
// once. actorUserID is the signed-out user, or "" for a system action.
func (m *Manager) after(ctx context.Context, devs []revoked, actorUserID, reason string) {
	if len(devs) == 0 {
		return
	}
	workspaces := map[string]bool{}
	for _, d := range devs {
		workspaces[d.workspaceID] = true
		if m.audit != nil {
			m.audit(ctx, audit.Entry{
				TenantID:    d.workspaceID,
				ActorUserID: actorUserID,
				Action:      "device.revoked",
				TargetType:  "client_device",
				TargetID:    d.deviceID,
				Details: map[string]any{
					"source":  "session",
					"reason":  reason,
					"user_id": d.userID,
				},
			})
		}
	}
	box := m.notifier.Load()
	if box == nil || box.n == nil {
		log.Printf("devicesession: no policy notifier; %d revoked device(s) reach connectors via the heartbeat compile", len(devs))
		return
	}
	for ws := range workspaces {
		if err := box.n.NotifyPolicyChange(ctx, ws); err != nil {
			log.Printf("devicesession: policy notify for workspace %s failed: %v", ws, err)
		}
	}
}
