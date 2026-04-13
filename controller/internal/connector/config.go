package connector

import "time"

// Config holds all tunable settings for the connector subsystem.
// Populated in main.go from environment variables in a later phase.
type Config struct {
	CertTTL             time.Duration
	EnrollmentTokenTTL  time.Duration
	HeartbeatInterval   time.Duration
	DisconnectThreshold time.Duration
	GRPCPort            string
	JWTSecret           string
}
