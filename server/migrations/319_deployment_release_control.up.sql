CREATE TABLE deployment_release_control (
    singleton BOOLEAN NOT NULL DEFAULT TRUE CHECK (singleton),
    active_generation BIGINT NOT NULL DEFAULT 1 CHECK (active_generation > 0),
    claims_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    admission_open BOOLEAN NOT NULL DEFAULT FALSE,
    worker_owner TEXT,
    worker_heartbeat_at TIMESTAMPTZ,
    drain_requested BOOLEAN NOT NULL DEFAULT FALSE,
    worker_in_flight BIGINT NOT NULL DEFAULT 0 CHECK (worker_in_flight >= 0),
    worker_leases BIGINT NOT NULL DEFAULT 0 CHECK (worker_leases >= 0),
    drain_zero_since TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO deployment_release_control (singleton) VALUES (TRUE);
