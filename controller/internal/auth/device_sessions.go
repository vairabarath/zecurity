package auth

import (
	"context"
	"errors"
	"time"
)

// ── PENDING-09 C-1: a device's live tunnels end when its login session ends ──
//
// The auth service owns sessions; the client devices enrolled under a session
// live elsewhere (internal/devicesession). SessionObserver is how auth tells
// that owner about session lifecycle events. Every method is best effort and
// must return promptly: the session change has already happened.

// SessionObserver receives session lifecycle events (Config.Sessions).
type SessionObserver interface {
	// SessionRefreshed reports a successful refresh of (userID, sid); the
	// session now idles out at idleUntil.
	SessionRefreshed(ctx context.Context, userID, sid string, idleUntil time.Time)
	// SessionEnded reports that one session ended explicitly (sign-out, or
	// its max lifetime refused at refresh). reason is "signed_out" or
	// "session_expired".
	SessionEnded(ctx context.Context, userID, sid, reason string)
	// UserSessionsEnded reports that every session of the user was ended
	// server-side (generation bump, failed lifecycle check at refresh).
	UserSessionsEnded(ctx context.Context, userID, reason string)
}

// SessionDeadlines are a live session's idle and absolute deadlines. A zero
// MaxUntil means the session has no absolute cap (legacy session).
type SessionDeadlines struct {
	IdleUntil time.Time
	MaxUntil  time.Time
}

// ErrSessionNotFound means the session does not exist (ended or expired).
var ErrSessionNotFound = errors.New("auth: session not found")

// SessionDeadlines returns the deadlines of the live session (userID, sid),
// or ErrSessionNotFound. Used at device enrollment to bind the device to its
// session (PENDING-09 D-1).
func (s *serviceImpl) SessionDeadlines(ctx context.Context, userID, sid string) (SessionDeadlines, error) {
	if userID == "" || sid == "" {
		return SessionDeadlines{}, ErrSessionNotFound
	}
	stored, found, err := s.redisClient.GetRefreshSession(ctx, userID, sid)
	if err != nil {
		return SessionDeadlines{}, err
	}
	if !found {
		return SessionDeadlines{}, ErrSessionNotFound
	}
	d := SessionDeadlines{IdleUntil: time.Now().Add(s.refreshTTL())}
	if stored.MaxLifetimeAt != 0 {
		d.MaxUntil = time.Unix(stored.MaxLifetimeAt, 0)
		if time.Now().After(d.MaxUntil) {
			return SessionDeadlines{}, ErrSessionNotFound
		}
	}
	return d, nil
}

func (s *serviceImpl) sessionRefreshed(ctx context.Context, userID, sid string) {
	if s.cfg.Sessions != nil && sid != "" {
		s.cfg.Sessions.SessionRefreshed(ctx, userID, sid, time.Now().Add(s.refreshTTL()))
	}
}

func (s *serviceImpl) sessionEnded(ctx context.Context, userID, sid, reason string) {
	if s.cfg.Sessions != nil && sid != "" {
		s.cfg.Sessions.SessionEnded(ctx, userID, sid, reason)
	}
}

func (s *serviceImpl) userSessionsEnded(ctx context.Context, userID, reason string) {
	if s.cfg.Sessions != nil {
		s.cfg.Sessions.UserSessionsEnded(ctx, userID, reason)
	}
}
