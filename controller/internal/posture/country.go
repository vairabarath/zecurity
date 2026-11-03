package posture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/geoip"
)

// CheckNetworkCountry is a server-side check (PENDING-20 G1): the country of
// the address the controller last saw the device connect from, judged against
// the requirement's allow and deny lists. A client can never report it.
const CheckNetworkCountry = "zecurity.network.country"

// MaxCountryRuleEntries caps each list (there are fewer than 250 countries).
const MaxCountryRuleEntries = 250

// ErrInvalidCountryRule is wrapped by every country-rule validation error;
// the message after it is safe to show an admin.
var ErrInvalidCountryRule = errors.New("invalid country rule")

// CountryRule is the params of a CheckNetworkCountry requirement (G-2, G-8):
// deny wins over allow; an empty allow list means "any country not denied".
// Unknown never passes (G-4).
type CountryRule struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// NormalizeCountryRule upper-cases, de-duplicates and sorts both lists, and
// refuses invalid codes, a rule with both lists empty (it would allow every
// known country silently) and a code in both lists (ambiguous).
func NormalizeCountryRule(rule CountryRule) (CountryRule, error) {
	norm := func(name string, in []string) ([]string, error) {
		if len(in) > MaxCountryRuleEntries {
			return nil, fmt.Errorf("%w: %s list has more than %d entries", ErrInvalidCountryRule, name, MaxCountryRuleEntries)
		}
		out := make([]string, 0, len(in))
		for _, c := range in {
			c = strings.ToUpper(strings.TrimSpace(c))
			if !geoip.ValidCountryCode(c) {
				return nil, fmt.Errorf("%w: %q is not an ISO 3166-1 alpha-2 country code", ErrInvalidCountryRule, c)
			}
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
		slices.Sort(out)
		return out, nil
	}
	allow, err := norm("allow", rule.Allow)
	if err != nil {
		return CountryRule{}, err
	}
	deny, err := norm("deny", rule.Deny)
	if err != nil {
		return CountryRule{}, err
	}
	if len(allow) == 0 && len(deny) == 0 {
		return CountryRule{}, fmt.Errorf("%w: list at least one country to allow or deny", ErrInvalidCountryRule)
	}
	for _, c := range allow {
		if slices.Contains(deny, c) {
			return CountryRule{}, fmt.Errorf("%w: %s is in both the allow and the deny list", ErrInvalidCountryRule, c)
		}
	}
	return CountryRule{Allow: allow, Deny: deny}, nil
}

// Allows reports whether a device observed in country passes the rule.
// country == "" (unknown) never passes (G-4).
func (r CountryRule) Allows(country string) bool {
	if country == "" || slices.Contains(r.Deny, country) {
		return false
	}
	return len(r.Allow) == 0 || slices.Contains(r.Allow, country)
}

// countryRuleFromParams decodes a stored requirement's params.
func countryRuleFromParams(raw []byte) (*CountryRule, error) {
	var rule CountryRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		return nil, fmt.Errorf("decode country rule: %w", err)
	}
	return &rule, nil
}

// NetworkObservation is the country the controller last saw a device in.
type NetworkObservation struct {
	DeviceID      uuid.UUID
	Country       string // "" when unknown
	NetworkPrefix netip.Prefix
	ObservedAt    time.Time
	CountrySince  time.Time
	GeoIPBuild    *time.Time
}

// RecordNetworkObservation stores a device's latest sighting. It reports
// whether the country changed (or this is the first sighting), which is when
// the device's profiles must be re-evaluated. Sightings that don't change the
// country only move observed_at.
func (s *Store) RecordNetworkObservation(
	ctx context.Context,
	workspaceID, deviceID uuid.UUID,
	lookup geoip.Lookup,
	now time.Time,
) (bool, error) {
	var prefix any
	if lookup.Prefix.IsValid() {
		prefix = lookup.Prefix.String()
	}
	var build any
	if !lookup.Build.IsZero() {
		build = lookup.Build
	}
	var previous *string
	err := s.pool.QueryRow(ctx,
		`WITH prev AS (
		   SELECT country FROM device_network_observations
		    WHERE device_id = $1 AND workspace_id = $2
		 )
		 INSERT INTO device_network_observations
		   (device_id, workspace_id, country, network_prefix, observed_at, country_since, geoip_build)
		 VALUES ($1, $2, $3, $4::cidr, $5, $5, $6)
		 ON CONFLICT (device_id) DO UPDATE SET
		   country        = EXCLUDED.country,
		   network_prefix = EXCLUDED.network_prefix,
		   observed_at    = EXCLUDED.observed_at,
		   geoip_build    = EXCLUDED.geoip_build,
		   country_since  = CASE WHEN device_network_observations.country = EXCLUDED.country
		                         THEN device_network_observations.country_since
		                         ELSE EXCLUDED.observed_at END
		 WHERE device_network_observations.workspace_id = EXCLUDED.workspace_id
		 RETURNING (SELECT country FROM prev)`,
		deviceID, workspaceID, lookup.Country, prefix, now, build,
	).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		// The row belongs to another workspace: refuse rather than move it.
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("record network observation: %w", err)
	}
	return previous == nil || *previous != lookup.Country, nil
}

// LatestNetworkObservation returns the device's last sighting, or ErrNotFound.
func (s *Store) LatestNetworkObservation(ctx context.Context, workspaceID, deviceID uuid.UUID) (NetworkObservation, error) {
	var (
		o      NetworkObservation
		prefix *netip.Prefix
	)
	err := s.pool.QueryRow(ctx,
		`SELECT device_id, country, network_prefix, observed_at, country_since, geoip_build
		   FROM device_network_observations
		  WHERE device_id = $1 AND workspace_id = $2`,
		deviceID, workspaceID,
	).Scan(&o.DeviceID, &o.Country, &prefix, &o.ObservedAt, &o.CountrySince, &o.GeoIPBuild)
	if errors.Is(err, pgx.ErrNoRows) {
		return NetworkObservation{}, ErrNotFound
	}
	if err != nil {
		return NetworkObservation{}, fmt.Errorf("load network observation: %w", err)
	}
	if prefix != nil {
		o.NetworkPrefix = *prefix
	}
	return o, nil
}

// countryObservation turns the last sighting into the observation the
// country check is evaluated against. A missing or stale (older than
// MaxReportAge) sighting, or an unknown country, is UNKNOWN: it never passes.
func countryObservation(reportID uuid.UUID, obs *NetworkObservation, now time.Time) Observation {
	status, detail := ObservationStatusUnknown, "device location unknown"
	switch {
	case obs == nil:
		detail = "device not seen yet"
	case now.Sub(obs.ObservedAt) > MaxReportAge:
		detail = "device location is stale"
	case obs.Country == "":
		detail = "device location unknown (no country for its address)"
	default:
		status, detail = ObservationStatusPass, obs.Country
	}
	return Observation{ReportID: reportID, CheckID: CheckNetworkCountry, Status: status, Detail: &detail}
}

// countryRequirementSatisfied judges the observed country against the rule.
func countryRequirementSatisfied(requirement Requirement, observation Observation) (bool, string) {
	if requirement.Countries == nil {
		return false, fmt.Sprintf("check %s has no country rule", CheckNetworkCountry)
	}
	if observation.Status != ObservationStatusPass || observation.Detail == nil {
		return false, fmt.Sprintf("check %s: %s", CheckNetworkCountry, stringValue(observation.Detail))
	}
	country := *observation.Detail
	if requirement.Countries.Allows(country) {
		return true, ""
	}
	if slices.Contains(requirement.Countries.Deny, country) {
		return false, fmt.Sprintf("check %s: %s is denied", CheckNetworkCountry, country)
	}
	return false, fmt.Sprintf("check %s: %s is not allowed", CheckNetworkCountry, country)
}

// UpdateCountryRule replaces the rule of a profile's country requirement in
// one transaction and bumps the profile revision (G-10). There is no window
// in which the profile has no country check, unlike remove-then-add. It
// returns the previous rule (for the audit row), or ErrNotFound when the
// profile has no country requirement in this workspace.
func (s *Store) UpdateCountryRule(ctx context.Context, workspaceID, profileID uuid.UUID, rule CountryRule) (CountryRule, error) {
	params, err := json.Marshal(rule)
	if err != nil {
		return CountryRule{}, fmt.Errorf("encode country rule: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CountryRule{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var previous []byte
	err = tx.QueryRow(ctx,
		`SELECT r.params
		   FROM device_profile_requirements r
		   JOIN device_profiles p ON p.id = r.profile_id
		  WHERE p.workspace_id = $1 AND p.id = $2 AND r.check_id = $3
		    FOR UPDATE OF r, p`,
		workspaceID, profileID, CheckNetworkCountry,
	).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return CountryRule{}, ErrNotFound
	}
	if err != nil {
		return CountryRule{}, fmt.Errorf("lock country requirement: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE device_profile_requirements SET params = $3
		  WHERE profile_id = $1 AND check_id = $2`,
		profileID, CheckNetworkCountry, params,
	); err != nil {
		return CountryRule{}, fmt.Errorf("update country rule: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE device_profiles SET revision = revision + 1, updated_at = NOW()
		  WHERE workspace_id = $1 AND id = $2`,
		workspaceID, profileID,
	); err != nil {
		return CountryRule{}, fmt.Errorf("update profile revision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CountryRule{}, fmt.Errorf("commit country rule: %w", err)
	}
	old, err := countryRuleFromParams(previous)
	if err != nil {
		return CountryRule{}, nil // the update committed; the audit just lacks the old lists
	}
	return *old, nil
}
