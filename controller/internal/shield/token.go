package shield

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

const enrollmentJTIPrefix = "shield:enrollment:jti:"

type EnrollmentClaims struct {
	jwt.RegisteredClaims
	ShieldID        string `json:"shield_id"`
	RemoteNetworkID string `json:"remote_network_id"`
	WorkspaceID     string `json:"workspace_id"`
	TrustDomain     string `json:"trust_domain"`
	CAFingerprint   string `json:"ca_fingerprint"`
	ConnectorID     string `json:"connector_id"`
	ConnectorAddr   string `json:"connector_addr"`
	InterfaceAddr   string `json:"interface_addr"`
}

func (s *service) GenerateShieldToken(
	ctx context.Context,
	remoteNetworkID, workspaceID, tenantID, shieldID, shieldName string,
) (tokenString string, installCommand string, err error) {
	_ = shieldName

	workspaceSlug, trustDomain, err := s.loadWorkspaceIdentity(ctx, workspaceID)
	if err != nil {
		return "", "", err
	}

	caFingerprint, err := s.loadCAFingerprint(ctx)
	if err != nil {
		return "", "", err
	}

	connectorID, connectorAddr, err := s.selectConnector(ctx, remoteNetworkID, tenantID)
	if err != nil {
		return "", "", err
	}

	interfaceAddr, err := s.assignInterfaceAddr(ctx, tenantID)
	if err != nil {
		return "", "", err
	}

	jti := uuid.NewString()
	now := time.Now()

	claims := EnrollmentClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Issuer:    appmeta.ControllerIssuer,
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.EnrollmentTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
		ShieldID:        shieldID,
		RemoteNetworkID: remoteNetworkID,
		WorkspaceID:     workspaceID,
		TrustDomain:     trustDomain,
		CAFingerprint:   caFingerprint,
		ConnectorID:     connectorID,
		ConnectorAddr:   connectorAddr,
		InterfaceAddr:   interfaceAddr,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err = token.SignedString([]byte(s.cfg.JWTSecret))
	if err != nil {
		return "", "", fmt.Errorf("sign shield enrollment token: %w", err)
	}

	if err := StoreShieldJTI(ctx, s.redis, jti, shieldID, s.cfg.EnrollmentTokenTTL); err != nil {
		return "", "", fmt.Errorf("store shield jti: %w", err)
	}

	_, err = s.db.Exec(ctx,
		`UPDATE shields
		    SET enrollment_token_jti = $1,
		        connector_id = $2,
		        trust_domain = $3,
		        interface_addr = $4,
		        updated_at = NOW()
		  WHERE id = $5
		    AND tenant_id = $6
		    AND remote_network_id = $7
		    AND status = 'pending'`,
		jti,
		connectorID,
		trustDomain,
		interfaceAddr,
		shieldID,
		tenantID,
		remoteNetworkID,
	)
	if err != nil {
		return "", "", fmt.Errorf("persist shield token state: %w", err)
	}

	controllerAddr := os.Getenv("CONTROLLER_ADDR")
	if controllerAddr == "" {
		controllerAddr = "localhost:9090"
	}

	controllerHTTPAddr := os.Getenv("CONTROLLER_HTTP_ADDR")
	if controllerHTTPAddr == "" {
		if i := strings.LastIndex(controllerAddr, ":"); i != -1 {
			controllerHTTPAddr = controllerAddr[:i] + ":8080"
		} else {
			controllerHTTPAddr = "localhost:8080"
		}
	}

	installCommand = fmt.Sprintf(
		"curl -fsSL https://raw.githubusercontent.com/vairabarath/zecurity/main/shield/scripts/shield-install.sh | sudo CONTROLLER_ADDR=%s CONTROLLER_HTTP_ADDR=%s ENROLLMENT_TOKEN=%s bash",
		controllerAddr,
		controllerHTTPAddr,
		tokenString,
	)

	_ = workspaceSlug

	return tokenString, installCommand, nil
}

func VerifyShieldToken(cfg Config, tokenString string) (*EnrollmentClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &EnrollmentClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}

		return []byte(cfg.JWTSecret), nil
	}, jwt.WithIssuer(appmeta.ControllerIssuer), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("verify shield enrollment token: %w", err)
	}

	claims, ok := token.Claims.(*EnrollmentClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid shield enrollment token claims")
	}

	return claims, nil
}

func StoreShieldJTI(ctx context.Context, rdb valkeycompat.Cmdable, jti, shieldID string, ttl time.Duration) error {
	return rdb.Set(ctx, enrollmentJTIPrefix+jti, shieldID, ttl).Err()
}

func BurnShieldJTI(ctx context.Context, rdb valkeycompat.Cmdable, jti string) (shieldID string, found bool, err error) {
	val, err := rdb.GetDel(ctx, enrollmentJTIPrefix+jti).Result()
	if err == valkeycompat.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("burn shield jti: %w", err)
	}
	return val, true, nil
}

func (s *service) loadWorkspaceIdentity(ctx context.Context, workspaceID string) (slug, trustDomain string, err error) {
	err = s.db.QueryRow(ctx,
		`SELECT slug, trust_domain FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&slug, &trustDomain)
	if err != nil {
		return "", "", fmt.Errorf("load workspace identity: %w", err)
	}

	return slug, trustDomain, nil
}

func (s *service) loadCAFingerprint(ctx context.Context) (string, error) {
	var certPEM string
	err := s.db.QueryRow(ctx,
		`SELECT certificate_pem FROM ca_intermediate LIMIT 1`,
	).Scan(&certPEM)
	if err != nil {
		return "", fmt.Errorf("load intermediate ca: %w", err)
	}

	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return "", fmt.Errorf("decode intermediate ca pem")
	}

	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

func (s *service) selectConnector(ctx context.Context, remoteNetworkID, tenantID string) (connectorID, connectorAddr string, err error) {
	var publicIP string

	err = s.db.QueryRow(ctx,
		`SELECT c.id, c.public_ip
		   FROM connectors c
		   LEFT JOIN shields s
		     ON s.connector_id = c.id
		    AND s.tenant_id = c.tenant_id
		    AND s.status <> 'revoked'
		  WHERE c.remote_network_id = $1
		    AND c.tenant_id = $2
		    AND c.status = 'active'
		    AND c.public_ip IS NOT NULL
		    AND c.public_ip <> ''
		  GROUP BY c.id, c.public_ip, c.last_heartbeat_at
		  ORDER BY COUNT(s.id) ASC, c.last_heartbeat_at DESC NULLS LAST
		  LIMIT 1`,
		remoteNetworkID,
		tenantID,
	).Scan(&connectorID, &publicIP)
	if err != nil {
		return "", "", fmt.Errorf("select active connector: %w", err)
	}

	connectorAddr = net.JoinHostPort(publicIP, "9091")
	return connectorID, connectorAddr, nil
}

func (s *service) assignInterfaceAddr(ctx context.Context, tenantID string) (string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT interface_addr
		   FROM shields
		  WHERE tenant_id = $1
		    AND interface_addr IS NOT NULL`,
		tenantID,
	)
	if err != nil {
		return "", fmt.Errorf("load used shield interface addresses: %w", err)
	}
	defer rows.Close()

	used := make(map[string]struct{})
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return "", fmt.Errorf("scan shield interface address: %w", err)
		}
		used[addr] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate shield interface addresses: %w", err)
	}

	prefix, err := netip.ParsePrefix(appmeta.ShieldInterfaceCIDR)
	if err != nil {
		return "", fmt.Errorf("parse shield interface cidr: %w", err)
	}

	addr := prefix.Addr().Next()
	for prefix.Contains(addr) {
		candidate := addr.String() + "/32"
		if _, ok := used[candidate]; !ok {
			return candidate, nil
		}
		addr = addr.Next()
	}

	return "", fmt.Errorf("no free shield interface addresses available in %s", appmeta.ShieldInterfaceCIDR)
}
