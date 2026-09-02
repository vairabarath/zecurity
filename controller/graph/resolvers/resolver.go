package resolvers

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/connector"
	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/resource"
	"github.com/yourorg/ztna/controller/internal/scim"
	"github.com/yourorg/ztna/controller/internal/shield"
	"github.com/yourorg/ztna/controller/internal/permission"
	"github.com/yourorg/ztna/controller/internal/transport"
)

// Resolver holds shared dependencies for all resolvers.
type Resolver struct {
	TenantDB          *db.TenantDB
	AuthService       auth.Service
	ConnectorCfg      connector.Config
	ConnectorRegistry *connector.ConnectorRegistry
	ShieldSvc         shield.Service
	ResourceCfg       resource.Config
	Redis             valkeycompat.Cmdable
	Pool              *pgxpool.Pool
	InvitationStore   *invitation.Store
	InvitationEmailer *invitation.Emailer
	PolicyStore       *policy.Store
	PolicyNotifier    *policy.Notifier
	PostureStore      *posture.Store
	PostureEvaluator  *posture.Evaluator
	TransportNotifier *transport.Notifier

	// IdpStore backs the identity-provider admin API (PENDING-04 Phase 6).
	IdpStore *idp.Store
	// ScimStore backs the SCIM bearer-token API (Sprint 17 / ADR-025).
	ScimStore *scim.Store
	// PermissionStore backs the explicit fine-grained permission primitive
	// (Sprint 17 / ADR-025 Phase 3, e.g. identity.mapping.break_glass).
	PermissionStore *permission.Store
	// Revoker performs session-generation revocation when a connection is
	// disabled/deleted (every user of that connection loses their login path).
	Revoker *identity.Revoker
	// BreakGlassEmails are admins who may always authenticate via the platform
	// IdP, so a workspace can never lock itself out. Consulted by the no-lockout
	// guard; from IDP_BREAK_GLASS_EMAILS. See ADR-024 §5.
	BreakGlassEmails map[string]bool
}
