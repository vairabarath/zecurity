package posture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound             = errors.New("posture record not found")
	ErrDuplicateReport      = errors.New("duplicate posture report")
	ErrDuplicateRequirement = errors.New("duplicate posture requirement")
	ErrDuplicateBinding     = errors.New("duplicate posture resource binding")
	ErrWorkspaceMismatch    = errors.New("workspace mismatch")
	ErrLastRequirement      = errors.New("cannot remove the last requirement from an enforced profile")
	ErrInvalidMode          = errors.New("invalid device profile mode")
)

const (
	ModeAudit   = "audit"
	ModeEnforce = "enforce"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

type Report struct {
	ID            uuid.UUID
	ReportID      string
	DeviceID      uuid.UUID
	WorkspaceID   uuid.UUID
	ClientVersion string
	OSInfo        json.RawMessage
	ReportedAt    time.Time
	ReceivedAt    time.Time
	CreatedAt     time.Time
	Observations  []Observation
}

type Observation struct {
	ID        uuid.UUID
	ReportID  uuid.UUID
	CheckID   string
	Status    string
	Detail    *string
	CreatedAt time.Time
}

type Profile struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Name        string
	Mode        string
	Revision    int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Requirement struct {
	ID               uuid.UUID
	ProfileID        uuid.UUID
	CheckID          string
	AllowUnsupported bool
}

type ResourceBinding struct {
	ID          uuid.UUID
	ResourceID  uuid.UUID
	ProfileID   uuid.UUID
	WorkspaceID uuid.UUID
}

type Evaluation struct {
	DeviceID         uuid.UUID
	ProfileID        uuid.UUID
	WorkspaceID      uuid.UUID
	Satisfied        bool
	ProfileRevision  int64
	Reason           *string
	EvaluatedAt      time.Time
	ReportID         *uuid.UUID
	ReportReceivedAt *time.Time
}

// InsertReport writes the report and all observations atomically.
//
// Report.ReportID is the client-generated idempotency key.
// Report.ID is ignored because the database generates the internal UUID.
func (s *Store) InsertReport(
	ctx context.Context,
	workspaceID uuid.UUID,
	deviceID uuid.UUID,
	report Report,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin insert posture report: %w", err)
	}
	defer tx.Rollback(ctx)

	var reportRowID uuid.UUID

	err = tx.QueryRow(
		ctx,
		`INSERT INTO device_posture_reports(
		report_id,
		device_id,
		workspace_id,
		client_version,
		os_info,
		reported_at
		)
		SELECT
		$1,
		d.id,
		d.workspace_id,
		$4,
		$5,
		$6
		FROM client_devices d
		WHERE d.id = $2
		AND d.workspace_id = $3
		AND d.revoked_at IS NULL
		RETURNING id`,
		report.ReportID,
		deviceID,
		workspaceID,
		report.ClientVersion,
		report.OSInfo,
		report.ReportedAt,
	).Scan(&reportRowID)

	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkspaceMismatch
	}
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateReport
		}
		return fmt.Errorf("insert posture report: %w", err)
	}
	for _, observation := range report.Observations {
		_, err = tx.Exec(
			ctx,
			`INSERT INTO device_posture_observations (
		     report_id,
		     check_id,
		     status,
		     detail
		 )
		 VALUES ($1, $2, $3, $4)`,
			reportRowID,
			observation.CheckID,
			observation.Status,
			observation.Detail,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrDuplicateReport
			}

			return fmt.Errorf(
				"insert posture observation %q: %w",
				observation.CheckID,
				err,
			)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit posture report: %w", err)
	}

	return nil
}
func (s *Store) LatestReport(
	ctx context.Context,
	workspaceID uuid.UUID,
	deviceID uuid.UUID,
) (*Report, error) {
	report := &Report{}

	err := s.pool.QueryRow(
		ctx,
		`SELECT id,
	        report_id,
	        device_id,
	        workspace_id,
	        client_version,
	        os_info,
	        reported_at,
	        received_at,
	        created_at
	   FROM device_posture_reports
	  WHERE workspace_id = $1
	    AND device_id = $2
	  ORDER BY received_at DESC
	  LIMIT 1`,
		workspaceID,
		deviceID,
	).Scan(
		&report.ID,
		&report.ReportID,
		&report.DeviceID,
		&report.WorkspaceID,
		&report.ClientVersion,
		&report.OSInfo,
		&report.ReportedAt,
		&report.ReceivedAt,
		&report.CreatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load latest posture report: %w", err)
	}

	return report, nil
}

func (s *Store) ListObservations(
	ctx context.Context,
	reportID uuid.UUID,
) ([]Observation, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT id,
		        report_id,
		        check_id,
		        status,
		        detail,
		        created_at
		   FROM device_posture_observations
		  WHERE report_id = $1
		  ORDER BY check_id`,
		reportID,
	)
	if err != nil {
		return nil, fmt.Errorf("list posture observations: %w", err)
	}
	defer rows.Close()

	observations := make([]Observation, 0)

	for rows.Next() {
		var observation Observation

		if err := rows.Scan(
			&observation.ID,
			&observation.ReportID,
			&observation.CheckID,
			&observation.Status,
			&observation.Detail,
			&observation.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan posture observation: %w", err)
		}

		observations = append(observations, observation)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate posture observations: %w", err)
	}

	return observations, nil
}

func (s *Store) CreateProfile(
	ctx context.Context,
	workspaceID uuid.UUID,
	name string,
) (*Profile, error) {
	profile := &Profile{}

	err := s.pool.QueryRow(
		ctx,
		`INSERT INTO device_profiles (
		     workspace_id,
		     name
		 )
		 VALUES ($1, $2)
		 RETURNING id,
		           workspace_id,
		           name,
		           mode,
		           revision,
		           created_at,
		           updated_at`,
		workspaceID,
		name,
	).Scan(
		&profile.ID,
		&profile.WorkspaceID,
		&profile.Name,
		&profile.Mode,
		&profile.Revision,
		&profile.CreatedAt,
		&profile.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create device profile: %w", err)
	}

	return profile, nil
}

func (s *Store) GetProfile(
	ctx context.Context,
	workspaceID,
	profileID uuid.UUID,
) (*Profile, error) {
	profile := &Profile{}

	err := s.pool.QueryRow(
		ctx,
		`SELECT id,
	        workspace_id,
	        name,
	        mode,
	        revision,
	        created_at,
	        updated_at
	   FROM device_profiles
	  WHERE workspace_id = $1
	    AND id = $2`,
		workspaceID,
		profileID,
	).Scan(
		&profile.ID,
		&profile.WorkspaceID,
		&profile.Name,
		&profile.Mode,
		&profile.Revision,
		&profile.CreatedAt,
		&profile.UpdatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get device profile: %w", err)
	}

	return profile, nil
}

func (s *Store) ListProfiles(
	ctx context.Context,
	workspaceID uuid.UUID,
) ([]Profile, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT id,
		        workspace_id,
		        name,
		        mode,
		        revision,
		        created_at,
		        updated_at
		   FROM device_profiles
		  WHERE workspace_id = $1
		  ORDER BY name`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list device profiles: %w", err)
	}
	defer rows.Close()

	profiles := make([]Profile, 0)

	for rows.Next() {
		var profile Profile

		if err := rows.Scan(
			&profile.ID,
			&profile.WorkspaceID,
			&profile.Name,
			&profile.Mode,
			&profile.Revision,
			&profile.CreatedAt,
			&profile.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan device profile: %w", err)
		}

		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate device profiles: %w", err)
	}
	return profiles, nil
}

func (s *Store) UpdateProfileMode(
	ctx context.Context,
	workspaceID,
	profileID uuid.UUID,
	mode string,
) (*Profile, error) {
	profile := &Profile{}

	if mode != ModeAudit && mode != ModeEnforce {
		return nil, ErrInvalidMode
	}

	err := s.pool.QueryRow(
		ctx,
		`UPDATE device_profiles
		SET mode = $3,
		updated_at = NOW()
		WHERE workspace_id = $1
		AND id = $2
		RETURNING
		 id,
		workspace_id,
		name,
		mode,
		revision,
		created_at,
		updated_at;`,

		workspaceID,
		profileID,
		mode,
	).Scan(
		&profile.ID,
		&profile.WorkspaceID,
		&profile.Name,
		&profile.Mode,
		&profile.Revision,
		&profile.CreatedAt,
		&profile.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update device profile: %w", err)
	}
	return profile, nil
}

func (s *Store) DeleteProfile(
	ctx context.Context,
	workspaceID,
	profileID uuid.UUID,
) error {
	cmdTag, err := s.pool.Exec(
		ctx,
		`DELETE FROM device_profiles
	WHERE workspace_id = $1
		AND id = $2
	`,
		workspaceID,
		profileID,
	)

	if err != nil {
		return fmt.Errorf("delete device profile: %w", err)
	}

	if cmdTag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AddRequirement(
	ctx context.Context,
	workspaceID,
	profileID uuid.UUID,
	requirement Requirement,
) error {
	tx, err := s.pool.Begin(ctx)

	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer tx.Rollback(ctx)

	var dummy int

	err = tx.QueryRow(
		ctx,
		`SELECT 1
		FROM device_profiles
		WHERE workspace_id = $1
		AND id = $2
		FOR UPDATE
		`,
		workspaceID,
		profileID,
	).Scan(&dummy)

	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}

	if err != nil {
		return fmt.Errorf("check device profile: %w", err)
	}
	_, err = tx.Exec(
		ctx,
		`INSERT INTO device_profile_requirements
	(profile_id, check_id, allow_unsupported)
	VALUES ($1, $2, $3)`,
		profileID,
		requirement.CheckID,
		requirement.AllowUnsupported,
	)

	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateRequirement
		}

		return fmt.Errorf("insert requirement: %w", err)
	}
	_, err = tx.Exec(
		ctx,
		`UPDATE device_profiles
	   SET revision = revision + 1,
	       updated_at = NOW()
	 WHERE workspace_id = $1
	   AND id = $2`,
		workspaceID,
		profileID,
	)

	if err != nil {
		return fmt.Errorf("update profile revision: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit add requirement: %w", err)
	}

	return nil
}

func (s *Store) RemoveRequirement(
	ctx context.Context,
	workspaceID,
	profileID uuid.UUID,
	checkID string,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the profile row so mode/revision can't change while we work.
	var mode string

	err = tx.QueryRow(
		ctx,
		`
		SELECT mode
		FROM device_profiles
		WHERE workspace_id = $1
		  AND id = $2
		FOR UPDATE
		`,
		workspaceID,
		profileID,
	).Scan(&mode)

	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load device profile: %w", err)
	}

	// Remove the requirement.
	cmdTag, err := tx.Exec(
		ctx,
		`
		DELETE FROM device_profile_requirements
		WHERE profile_id = $1
		  AND check_id = $2
		`,
		profileID,
		checkID,
	)
	if err != nil {
		return fmt.Errorf("delete requirement: %w", err)
	}

	if cmdTag.RowsAffected() == 0 {
		return ErrNotFound
	}

	// Count remaining requirements.
	var remaining int

	err = tx.QueryRow(
		ctx,
		`
		SELECT COUNT(*)
		FROM device_profile_requirements
		WHERE profile_id = $1
		`,
		profileID,
	).Scan(&remaining)

	if err != nil {
		return fmt.Errorf("count requirements: %w", err)
	}

	// Enforced profiles must always have at least one requirement.
	if mode == ModeEnforce && remaining == 0 {
		return ErrLastRequirement
	}

	// Bump the revision.
	_, err = tx.Exec(
		ctx,
		`
		UPDATE device_profiles
		SET revision = revision + 1,
		    updated_at = NOW()
		WHERE workspace_id = $1
		  AND id = $2
		`,
		workspaceID,
		profileID,
	)

	if err != nil {
		return fmt.Errorf("update profile revision: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit remove requirement: %w", err)
	}

	return nil
}

func (s *Store) ListRequirements(
	ctx context.Context,
	workspaceID,
	profileID uuid.UUID,
) ([]Requirement, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT r.id,
		        r.profile_id,
		        r.check_id,
		        r.allow_unsupported
		   FROM device_profile_requirements r
		   JOIN device_profiles p ON p.id = r.profile_id
		  WHERE p.workspace_id = $1
		    AND p.id = $2
		  ORDER BY r.check_id`,
		workspaceID,
		profileID,
	)
	if err != nil {
		return nil, fmt.Errorf("list posture requirements: %w", err)
	}
	defer rows.Close()

	requirements := make([]Requirement, 0)
	for rows.Next() {
		var requirement Requirement
		if err := rows.Scan(
			&requirement.ID,
			&requirement.ProfileID,
			&requirement.CheckID,
			&requirement.AllowUnsupported,
		); err != nil {
			return nil, fmt.Errorf("scan posture requirement: %w", err)
		}
		requirements = append(requirements, requirement)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate posture requirements: %w", err)
	}

	return requirements, nil
}

func (s *Store) CreateResourceBinding(
	ctx context.Context,
	workspaceID,
	resourceID,
	profileID uuid.UUID,
) (*ResourceBinding, error) {

	binding := &ResourceBinding{}

	err := s.pool.QueryRow(
		ctx,
		`
		INSERT INTO resource_profile_bindings (
			resource_id,
			profile_id,
			workspace_id
		)
		SELECT
			r.id,
			p.id,
			$1
		FROM resources r
		JOIN device_profiles p
			ON p.id = $3
		WHERE r.id = $2
		  AND r.tenant_id = $1
		  AND p.workspace_id = $1
		RETURNING
			id,
			resource_id,
			profile_id,
			workspace_id
		`,
		workspaceID,
		resourceID,
		profileID,
	).Scan(
		&binding.ID,
		&binding.ResourceID,
		&binding.ProfileID,
		&binding.WorkspaceID,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkspaceMismatch
	}

	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateBinding
		}
		return nil, fmt.Errorf("create resource binding: %w", err)
	}

	return binding, nil
}

func (s *Store) DeleteResourceBinding(
	ctx context.Context,
	workspaceID,
	resourceID,
	profileID uuid.UUID,
) error {

	cmdTag, err := s.pool.Exec(
		ctx,
		`
		DELETE FROM resource_profile_bindings
		WHERE workspace_id = $1
		  AND resource_id = $2
		  AND profile_id = $3
		`,
		workspaceID,
		resourceID,
		profileID,
	)

	if err != nil {
		return fmt.Errorf("delete resource binding: %w", err)
	}

	if cmdTag.RowsAffected() == 0 {
		return ErrNotFound
	}

	return nil
}

func (s *Store) ListResourceBindings(
	ctx context.Context,
	workspaceID,
	profileID uuid.UUID,
) ([]ResourceBinding, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT id, resource_id, profile_id, workspace_id
		   FROM resource_profile_bindings
		  WHERE workspace_id = $1
		    AND profile_id = $2
		  ORDER BY resource_id`,
		workspaceID,
		profileID,
	)
	if err != nil {
		return nil, fmt.Errorf("list resource profile bindings: %w", err)
	}
	defer rows.Close()

	bindings := make([]ResourceBinding, 0)
	for rows.Next() {
		var binding ResourceBinding
		if err := rows.Scan(
			&binding.ID,
			&binding.ResourceID,
			&binding.ProfileID,
			&binding.WorkspaceID,
		); err != nil {
			return nil, fmt.Errorf("scan resource profile binding: %w", err)
		}
		bindings = append(bindings, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate resource profile bindings: %w", err)
	}

	return bindings, nil
}

func (s *Store) LatestEvaluation(
	ctx context.Context,
	workspaceID,
	deviceID,
	profileID uuid.UUID,
) (*Evaluation, error) {

	e := &Evaluation{}

	err := s.pool.QueryRow(
		ctx,
		`
		SELECT
			e.device_id,
			e.profile_id,
			e.workspace_id,
			e.satisfied,
			e.profile_revision,
			e.reason,
			e.evaluated_at,
			e.report_id,
			r.received_at
		FROM device_profile_evaluations e
		LEFT JOIN device_posture_reports r ON r.id = e.report_id
		WHERE e.workspace_id = $1
		  AND e.device_id = $2
		  AND e.profile_id = $3
		`,
		workspaceID,
		deviceID,
		profileID,
	).Scan(
		&e.DeviceID,
		&e.ProfileID,
		&e.WorkspaceID,
		&e.Satisfied,
		&e.ProfileRevision,
		&e.Reason,
		&e.EvaluatedAt,
		&e.ReportID,
		&e.ReportReceivedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load evaluation: %w", err)
	}

	return e, nil
}

func (s *Store) UpsertEvaluation(
	ctx context.Context,
	workspaceID,
	deviceID,
	profileID uuid.UUID,
	profileRevision int64,
	satisfied bool,
	reason *string,
	reportID *uuid.UUID,
) error {
	cmdTag, err := s.pool.Exec(
		ctx,
		`
		INSERT INTO device_profile_evaluations (
			device_id,
			profile_id,
			workspace_id,
			satisfied,
			profile_revision,
			reason,
			evaluated_at,
			report_id
		)
		SELECT
			d.id,
			p.id,
			$1,
			$4,
			$5,
			$6,
			NOW(),
			$7
		FROM client_devices d
		JOIN device_profiles p
		  ON p.id = $3
		 AND p.workspace_id = $1
		LEFT JOIN device_posture_reports r
		  ON r.id = $7
		 AND r.workspace_id = $1
		 AND r.device_id = $2
		WHERE d.id = $2
		  AND d.workspace_id = $1
		  AND ($7::uuid IS NULL OR r.id IS NOT NULL)
		ON CONFLICT (device_id, profile_id)
		DO UPDATE SET
			workspace_id = EXCLUDED.workspace_id,
			satisfied = EXCLUDED.satisfied,
			profile_revision = EXCLUDED.profile_revision,
			reason = EXCLUDED.reason,
			evaluated_at = EXCLUDED.evaluated_at,
			report_id = EXCLUDED.report_id
		`,
		workspaceID,
		deviceID,
		profileID,
		satisfied,
		profileRevision,
		reason,
		reportID,
	)

	if err != nil {
		return fmt.Errorf("upsert evaluation: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrWorkspaceMismatch
	}

	return nil
}

func (s *Store) EvaluationsForDevices(
	ctx context.Context,
	workspaceID uuid.UUID,
	deviceIDs []uuid.UUID,
) (map[uuid.UUID][]Evaluation, error) {
	evaluations := make(map[uuid.UUID][]Evaluation, len(deviceIDs))
	if len(deviceIDs) == 0 {
		return evaluations, nil
	}

	rows, err := s.pool.Query(
		ctx,
		`SELECT e.device_id,
		        e.profile_id,
		        e.workspace_id,
		        e.satisfied,
		        e.profile_revision,
		        e.reason,
		        e.evaluated_at,
		        e.report_id,
		        r.received_at
		   FROM device_profile_evaluations e
		   LEFT JOIN device_posture_reports r ON r.id = e.report_id
		  WHERE e.workspace_id = $1
		    AND e.device_id = ANY($2::uuid[])
		  ORDER BY e.device_id, e.profile_id`,
		workspaceID,
		deviceIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("list posture evaluations for devices: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var evaluation Evaluation
		if err := rows.Scan(
			&evaluation.DeviceID,
			&evaluation.ProfileID,
			&evaluation.WorkspaceID,
			&evaluation.Satisfied,
			&evaluation.ProfileRevision,
			&evaluation.Reason,
			&evaluation.EvaluatedAt,
			&evaluation.ReportID,
			&evaluation.ReportReceivedAt,
		); err != nil {
			return nil, fmt.Errorf("scan posture evaluation: %w", err)
		}
		evaluations[evaluation.DeviceID] = append(
			evaluations[evaluation.DeviceID],
			evaluation,
		)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate posture evaluations: %w", err)
	}

	return evaluations, nil
}

func (s *Store) ReportByClientID(
	ctx context.Context,
	reportID string,
) (*Report, error) {
	report := &Report{}

	err := s.pool.QueryRow(
		ctx,
		`
		SELECT
			id,
			report_id,
			device_id,
			workspace_id,
			client_version,
			os_info,
			reported_at,
			received_at,
			created_at
		FROM device_posture_reports
		WHERE report_id = $1
		`,
		reportID,
	).Scan(
		&report.ID,
		&report.ReportID,
		&report.DeviceID,
		&report.WorkspaceID,
		&report.ClientVersion,
		&report.OSInfo,
		&report.ReportedAt,
		&report.ReceivedAt,
		&report.CreatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, fmt.Errorf(
			"load posture report by client id: %w",
			err,
		)
	}

	return report, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
