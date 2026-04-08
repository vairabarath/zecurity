package appmeta

import "testing"

func TestWorkspaceTrustDomain(t *testing.T) {
	tests := []struct {
		slug     string
		expected string
	}{
		{"acme", "ws-acme.zecurity.in"},
		{"test", "ws-test.zecurity.in"},
		{"prod-workspace", "ws-prod-workspace.zecurity.in"},
	}

	for _, tc := range tests {
		t.Run(tc.slug, func(t *testing.T) {
			got := WorkspaceTrustDomain(tc.slug)
			if got != tc.expected {
				t.Errorf("WorkspaceTrustDomain(%q) = %q, want %q", tc.slug, got, tc.expected)
			}
		})
	}
}

func TestConnectorSPIFFEID(t *testing.T) {
	tests := []struct {
		trustDomain string
		connectorID string
		expected    string
	}{
		{"ws-acme.zecurity.in", "abc-123", "spiffe://ws-acme.zecurity.in/connector/abc-123"},
		{"ws-test.zecurity.in", "conn-456", "spiffe://ws-test.zecurity.in/connector/conn-456"},
	}

	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			got := ConnectorSPIFFEID(tc.trustDomain, tc.connectorID)
			if got != tc.expected {
				t.Errorf("ConnectorSPIFFEID(%q, %q) = %q, want %q", tc.trustDomain, tc.connectorID, got, tc.expected)
			}
		})
	}
}

func TestSPIFFEConstants(t *testing.T) {
	// Verify constants are not empty
	if SPIFFEGlobalTrustDomain == "" {
		t.Error("SPIFFEGlobalTrustDomain must not be empty")
	}
	if SPIFFEControllerID == "" {
		t.Error("SPIFFEControllerID must not be empty")
	}
	if SPIFFETrustDomainPrefix == "" {
		t.Error("SPIFFETrustDomainPrefix must not be empty")
	}
	if SPIFFETrustDomainSuffix == "" {
		t.Error("SPIFFETrustDomainSuffix must not be empty")
	}
	if SPIFFERoleConnector == "" {
		t.Error("SPIFFERoleConnector must not be empty")
	}
	if SPIFFERoleAgent == "" {
		t.Error("SPIFFERoleAgent must not be empty")
	}
	if SPIFFERoleController == "" {
		t.Error("SPIFFERoleController must not be empty")
	}
	if PKIConnectorCNPrefix == "" {
		t.Error("PKIConnectorCNPrefix must not be empty")
	}
	if PKIAgentCNPrefix == "" {
		t.Error("PKIAgentCNPrefix must not be empty")
	}
}

func TestSPIFFEControllerIDFormat(t *testing.T) {
	want := "spiffe://" + SPIFFEGlobalTrustDomain + "/controller/global"
	if SPIFFEControllerID != want {
		t.Errorf("SPIFFEControllerID = %q, want %q", SPIFFEControllerID, want)
	}
}

func TestTrustDomainSuffixReferencesGlobalDomain(t *testing.T) {
	want := "." + SPIFFEGlobalTrustDomain
	if SPIFFETrustDomainSuffix != want {
		t.Errorf("SPIFFETrustDomainSuffix = %q, want %q", SPIFFETrustDomainSuffix, want)
	}
}
