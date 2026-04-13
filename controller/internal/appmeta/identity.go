package appmeta

// ── Sprint 1 constants (unchanged) ───────────────────────────────────────────
const (
	ProductName = "ZECURITY"

	ControllerIssuer = "zecurity-controller"

	PKIPlatformOrganization   = ProductName + " Platform"
	PKIWorkspaceOrganization  = ProductName + " Workspace"
	PKIRootCACommonName       = ProductName + " Root CA"
	PKIIntermediateCommonName = ProductName + " Intermediate CA"
)

// ── SPIFFE identity constants (connector sprint) ─────────────────────────────
//
// These are product-level identity constants. They define what ZECURITY is,
// not how it is deployed. They must NOT be overridable via config files or
// environment variables — a compromised config must not be able to redirect
// identity trust to a rogue domain.
//
// All packages that need SPIFFE strings import these. No package writes
// "zecurity.in", "ws-", or "connector" as a string literal directly.
const (
	// SPIFFEGlobalTrustDomain is the vendor-level trust domain.
	// Used for the controller certificate and as the root of all workspace
	// trust domains.
	SPIFFEGlobalTrustDomain = "zecurity.in"

	// SPIFFEControllerID is the full SPIFFE URI embedded in the controller's
	// TLS certificate. The Rust connector verifies this on every mTLS handshake.
	SPIFFEControllerID = "spiffe://" + SPIFFEGlobalTrustDomain + "/controller/global"

	// SPIFFETrustDomainPrefix and SPIFFETrustDomainSuffix form workspace trust
	// domains. Use WorkspaceTrustDomain(slug) — never concatenate manually.
	SPIFFETrustDomainPrefix = "ws-"
	SPIFFETrustDomainSuffix = "." + SPIFFEGlobalTrustDomain

	// SPIFFE role path segments — verified by UnarySPIFFEInterceptor.
	SPIFFERoleConnector  = "connector"
	SPIFFERoleAgent      = "agent" // future sprint — plumbed now
	SPIFFERoleController = "controller"

	// PKI cert subject CN prefixes — keeps cert naming consistent.
	PKIConnectorCNPrefix = "connector-" // CN = "connector-<connectorID>"
	PKIAgentCNPrefix     = "agent-"     // CN = "agent-<agentID>" — future
)

// WorkspaceTrustDomain derives the SPIFFE trust domain for a workspace.
//
// Example: slug "acme" → "ws-acme.zecurity.in"
//
// Every package that needs a workspace trust domain calls this function.
// The format is defined once here — nowhere else.
func WorkspaceTrustDomain(slug string) string {
	return SPIFFETrustDomainPrefix + slug + SPIFFETrustDomainSuffix
}

// ConnectorSPIFFEID builds the full SPIFFE URI for a connector certificate.
//
// Example: trustDomain "ws-acme.zecurity.in", connectorID "abc-123"
//
//	→ "spiffe://ws-acme.zecurity.in/connector/abc-123"
//
// Used in SignConnectorCert (Go) and enrollment.rs (Rust, mirrored).
func ConnectorSPIFFEID(trustDomain, connectorID string) string {
	return "spiffe://" + trustDomain + "/" + SPIFFERoleConnector + "/" + connectorID
}
