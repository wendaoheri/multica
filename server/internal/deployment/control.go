package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrGenerationMismatch = errors.New("deployment generation mismatch")
	ErrWorkerOwned        = errors.New("deployment worker generation already owned")
	ErrDrainIncomplete    = errors.New("deployment worker drain is incomplete")
)

const DrainObservationWindow = 60 * time.Second

type ControlSnapshot struct {
	ActiveGeneration int64      `json:"active_generation"`
	ClaimsEnabled    bool       `json:"claims_enabled"`
	AdmissionOpen    bool       `json:"admission_open"`
	WorkerOwner      string     `json:"worker_owner,omitempty"`
	WorkerHeartbeat  *time.Time `json:"worker_heartbeat_at,omitempty"`
	DrainRequested   bool       `json:"drain_requested"`
	WorkerInFlight   int64      `json:"worker_in_flight"`
	WorkerLeases     int64      `json:"worker_leases"`
	DrainZeroSince   *time.Time `json:"drain_zero_since,omitempty"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type ControlStore struct {
	pool *pgxpool.Pool
}

func NewControlStore(pool *pgxpool.Pool) *ControlStore { return &ControlStore{pool: pool} }

func scanSnapshot(row pgx.Row) (ControlSnapshot, error) {
	var s ControlSnapshot
	var owner *string
	if err := row.Scan(&s.ActiveGeneration, &s.ClaimsEnabled, &s.AdmissionOpen, &owner, &s.WorkerHeartbeat,
		&s.DrainRequested, &s.WorkerInFlight, &s.WorkerLeases, &s.DrainZeroSince, &s.UpdatedAt); err != nil {
		return ControlSnapshot{}, err
	}
	if owner != nil {
		s.WorkerOwner = *owner
	}
	return s, nil
}

func (s *ControlStore) Load(ctx context.Context) (ControlSnapshot, error) {
	return scanSnapshot(s.pool.QueryRow(ctx, `
		SELECT active_generation, claims_enabled, admission_open,
		       worker_owner, worker_heartbeat_at, drain_requested,
		       worker_in_flight, worker_leases, drain_zero_since, updated_at
		FROM deployment_release_control WHERE singleton = TRUE`))
}

// AdvanceGeneration permanently fences the previous worker owner and closes
// both task admission and claims. Re-opening either side is a separate,
// auditable operation after the new generation's smoke passes.
func (s *ControlStore) AdvanceGeneration(ctx context.Context, expected int64) (ControlSnapshot, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE deployment_release_control
		SET active_generation = active_generation + 1,
		    claims_enabled = FALSE,
		    admission_open = FALSE,
		    worker_owner = NULL,
		    worker_heartbeat_at = NULL,
		    drain_requested = FALSE,
		    worker_in_flight = 0,
		    worker_leases = 0,
		    drain_zero_since = NULL,
		    updated_at = NOW()
		WHERE singleton = TRUE AND active_generation = $1
		  AND worker_owner IS NULL
		  AND worker_in_flight = 0
		  AND worker_leases = 0
		RETURNING active_generation, claims_enabled, admission_open,
		          worker_owner, worker_heartbeat_at, drain_requested,
		          worker_in_flight, worker_leases, drain_zero_since, updated_at`, expected)
	snapshot, err := scanSnapshot(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ControlSnapshot{}, ErrGenerationMismatch
	}
	return snapshot, err
}

func (s *ControlStore) SetAdmission(ctx context.Context, generation int64, open bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployment_release_control
		SET admission_open = $2, updated_at = NOW()
		WHERE singleton = TRUE AND active_generation = $1`, generation, open)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrGenerationMismatch
	}
	return nil
}

// EnableClaims atomically binds the active generation to one worker instance.
// A second worker cannot become active merely by sharing the generation value.
func (s *ControlStore) EnableClaims(ctx context.Context, generation int64, owner string) error {
	if owner == "" {
		return fmt.Errorf("worker owner is required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployment_release_control
		SET claims_enabled = TRUE,
		    worker_owner = $2,
		    worker_heartbeat_at = NOW(),
		    updated_at = NOW()
		WHERE singleton = TRUE
		  AND active_generation = $1
		  AND drain_requested = FALSE
		  AND worker_in_flight = 0
		  AND worker_leases = 0
		  AND (worker_owner IS NULL OR worker_owner = $2)`, generation, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	snapshot, loadErr := s.Load(ctx)
	if loadErr != nil {
		return loadErr
	}
	if snapshot.ActiveGeneration != generation {
		return ErrGenerationMismatch
	}
	return ErrWorkerOwned
}

// Activate atomically opens admission and claims for exactly one generation
// and owner. There is no externally visible half-open state.
func (s *ControlStore) Activate(ctx context.Context, generation int64, owner string) error {
	if owner == "" {
		return fmt.Errorf("worker owner is required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployment_release_control
		SET admission_open = TRUE,
		    claims_enabled = TRUE,
		    worker_owner = $2,
		    worker_heartbeat_at = NOW(),
		    updated_at = NOW()
		WHERE singleton = TRUE
		  AND active_generation = $1
		  AND admission_open = FALSE
		  AND claims_enabled = FALSE
		  AND drain_requested = FALSE
		  AND worker_in_flight = 0
		  AND worker_leases = 0
		  AND (worker_owner IS NULL OR worker_owner = $2)`, generation, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrWorkerOwned
	}
	return nil
}

// Drain closes claims but deliberately retains owner. A replacement cannot be
// enabled until the old process has stopped all fenced operations and leases,
// reported zero continuously, and CompleteDrain releases ownership.
func (s *ControlStore) Drain(ctx context.Context, generation int64, owner string) error {
	if owner == "" {
		return fmt.Errorf("worker owner is required to drain")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployment_release_control
		SET claims_enabled = FALSE,
		    admission_open = FALSE,
		    drain_requested = TRUE,
		    drain_zero_since = CASE
		      WHEN worker_in_flight = 0 AND worker_leases = 0
		      THEN COALESCE(drain_zero_since, NOW()) ELSE NULL END,
		    updated_at = NOW()
		WHERE singleton = TRUE
		  AND active_generation = $1
		  AND worker_owner = $2`, generation, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrGenerationMismatch
	}
	return nil
}

func (s *ControlStore) CompleteDrain(ctx context.Context, generation int64, owner string, window time.Duration) error {
	if window <= 0 {
		window = DrainObservationWindow
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployment_release_control
		SET worker_owner = NULL,
		    worker_heartbeat_at = NULL,
		    drain_requested = FALSE,
		    drain_zero_since = NULL,
		    updated_at = NOW()
		WHERE singleton = TRUE
		  AND active_generation = $1
		  AND worker_owner = $2
		  AND claims_enabled = FALSE
		  AND admission_open = FALSE
		  AND drain_requested = TRUE
		  AND worker_in_flight = 0
		  AND worker_leases = 0
		  AND drain_zero_since IS NOT NULL
		  AND drain_zero_since <= NOW() - make_interval(secs => $3)`, generation, owner, window.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrDrainIncomplete
	}
	return nil
}

func (s *ControlStore) ReportActivity(ctx context.Context, generation int64, owner string, inFlight, leases int64) error {
	if inFlight < 0 || leases < 0 {
		return fmt.Errorf("activity counts cannot be negative")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployment_release_control
		SET worker_heartbeat_at = NOW(),
		    worker_in_flight = $3,
		    worker_leases = $4,
		    drain_zero_since = CASE
		      WHEN drain_requested AND $3 = 0 AND $4 = 0
		      THEN COALESCE(drain_zero_since, NOW()) ELSE NULL END,
		    updated_at = NOW()
		WHERE singleton = TRUE
		  AND active_generation = $1
		  AND worker_owner = $2`, generation, owner, inFlight, leases)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrGenerationMismatch
	}
	return nil
}

func (s *ControlStore) acquire(ctx context.Context, generation int64, owner string, lease bool) error {
	column := "worker_in_flight"
	if lease {
		column = "worker_leases"
	}
	query := fmt.Sprintf(`UPDATE deployment_release_control SET %s = %s + 1, updated_at = NOW()
		WHERE singleton = TRUE AND active_generation = $1 AND worker_owner = $2
		AND claims_enabled = TRUE AND drain_requested = FALSE`, column, column)
	tag, err := s.pool.Exec(ctx, query, generation, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrGenerationMismatch
	}
	return nil
}

func (s *ControlStore) release(ctx context.Context, generation int64, owner string, lease bool) error {
	column := "worker_in_flight"
	if lease {
		column = "worker_leases"
	}
	query := fmt.Sprintf(`UPDATE deployment_release_control SET %s = GREATEST(%s - 1, 0),
		drain_zero_since = CASE WHEN drain_requested AND
		  worker_in_flight - CASE WHEN $3 = FALSE THEN 1 ELSE 0 END <= 0 AND
		  worker_leases - CASE WHEN $3 = TRUE THEN 1 ELSE 0 END <= 0
		  THEN COALESCE(drain_zero_since, NOW()) ELSE NULL END, updated_at = NOW()
		WHERE singleton = TRUE AND active_generation = $1 AND worker_owner = $2`, column, column)
	tag, err := s.pool.Exec(ctx, query, generation, owner, lease)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrGenerationMismatch
	}
	return nil
}

func (s *ControlStore) AcquireOperation(ctx context.Context, generation int64, owner string) error {
	return s.acquire(ctx, generation, owner, false)
}
func (s *ControlStore) ReleaseOperation(ctx context.Context, generation int64, owner string) error {
	return s.release(ctx, generation, owner, false)
}
func (s *ControlStore) AcquireLease(ctx context.Context, generation int64, owner string) error {
	return s.acquire(ctx, generation, owner, true)
}
func (s *ControlStore) ReleaseLease(ctx context.Context, generation int64, owner string) error {
	return s.release(ctx, generation, owner, true)
}

func (s *ControlStore) Heartbeat(ctx context.Context, generation int64, owner string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployment_release_control
		SET worker_heartbeat_at = NOW(), updated_at = NOW()
		WHERE singleton = TRUE
		  AND active_generation = $1
		  AND worker_owner = $2`, generation, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrGenerationMismatch
	}
	return nil
}
