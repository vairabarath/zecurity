package pki

import (
	"crypto/x509"
	"fmt"
)

func auditCAConstraints(root, inter *x509.Certificate) error {
	if root.MaxPathLenZero || (root.MaxPathLen > 0 && root.MaxPathLen < 2) {
		return fmt.Errorf("pki audit: stored Root CA path length %d cannot support leaf→workspace→intermediate chains; see .zecurity-obs/Services/PKI.md remediation", root.MaxPathLen)
	}
	if inter.MaxPathLenZero {
		return fmt.Errorf("pki audit: stored Intermediate CA has MaxPathLen=0 — workspace chains can never validate; see .zecurity-obs/Services/PKI.md remediation")
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	if _, err := inter.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		return fmt.Errorf("pki audit: intermediate does not chain to stored root: %w", err)
	}
	return nil
}
