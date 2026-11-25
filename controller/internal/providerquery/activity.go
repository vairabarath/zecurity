package providerquery

// Tenant activity definitions shared by the M1 metric sampler (F23) and the
// usage collector (Sprint 25 U1, F32; D-57): one definition each, so a chart
// and a usage figure can never disagree about what was "live" or "active".
// SQL fragments over fixed aliases: w = workspaces, d = client_devices,
// c = connectors, sh = shields, and $1 = the instant being measured.
const (
	// CountedTenantSQL: a tenant the sampler and the collector count.
	CountedTenantSQL = "w.status = 'active'"
	// LiveClientSessionSQL: a client device with a live session at $1.
	LiveClientSessionSQL = "d.revoked_at IS NULL AND d.session_idle_until > $1 AND d.session_max_until > $1"
	// ActiveConnectorSQL / ActiveShieldSQL: an agent counted as online.
	ActiveConnectorSQL = "c.status = 'active'"
	ActiveShieldSQL    = "sh.status = 'active'"
)
