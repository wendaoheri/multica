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
)

type ControlSnapshot struct {
	ActiveGeneration int64      `json:"active_generation"`
	ClaimsEnabled    bool       `json:"claims_enabled"`
	AdmissionOpen    bool       `json:"admission_open"`
	WorkerOwner      string     `json:"worker_owner,omitempty"`
	WorkerHeartbeat  *time.Time `json:"worker_heartbeat_at,omitempty"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type ControlStore struct {
	pool *pgxpool.Pool
}

func NewControlStore(pool *pgxpool.Pool) *ControlStore { return &ControlStore{pool: pool} }

func scanSnapshot(row pgx.Row) (ControlSnapshot, error) {
	var s ControlSnapshot
	var owner *string
	if err := row.Scan(&s.ActiveGeneration, &s.ClaimsEnabled, &s.AdmissionOpen, &owner, &s.WorkerHeartbeat, &s.UpdatedAt); err != nil {
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
		       worker_owner, worker_heartbeat_at, updated_at
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
		    updated_at = NOW()
		WHERE singleton = TRUE AND active_generation = $1
		RETURNING active_generation, claims_enabled, admission_open,
		          worker_owner, worker_heartbeat_at, updated_at`, expected)
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

func (s *ControlStore) Drain(ctx context.Context, generation int64, owner string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployment_release_control
		SET claims_enabled = FALSE,
		    worker_owner = NULL,
		    worker_heartbeat_at = NULL,
		    updated_at = NOW()
		WHERE singleton = TRUE
		  AND active_generation = $1
		  AND ($2 = '' OR worker_owner = $2 OR worker_owner IS NULL)`, generation, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrGenerationMismatch
	}
	return nil
}

func (s *ControlStore) Heartbeat(ctx context.Context, generation int64, owner string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployment_release_control
		SET worker_heartbeat_at = NOW(), updated_at = NOW()
		WHERE singleton = TRUE
		  AND active_generation = $1
		  AND claims_enabled = TRUE
		  AND worker_owner = $2`, generation, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrGenerationMismatch
	}
	return nil
}
