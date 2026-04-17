package shield

import (
	"fmt"
	"strings"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

func ParseShieldSPIFFEID(uri string) (trustDomain, shieldID string, err error) {
	const prefix = "spiffe://"

	if !strings.HasPrefix(uri, prefix) {
		return "", "", fmt.Errorf("invalid SPIFFE URI %q", uri)
	}

	trimmed := strings.TrimPrefix(uri, prefix)
	segments := strings.Split(trimmed, "/")
	if len(segments) != 3 {
		return "", "", fmt.Errorf("invalid SPIFFE URI path %q", uri)
	}

	if segments[1] != appmeta.SPIFFERoleShield {
		return "", "", fmt.Errorf("expected SPIFFE role %q, got %q", appmeta.SPIFFERoleShield, segments[1])
	}
	if segments[0] == "" || segments[2] == "" {
		return "", "", fmt.Errorf("invalid SPIFFE URI %q", uri)
	}

	return segments[0], segments[2], nil
}
