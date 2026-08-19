CREATE TABLE deployment_release_control (
    singleton BOOLEAN NOT NULL DEFAULT TRUE CHECK (singleton),
    active_generation BIGINT NOT NULL DEFAULT 1 CHECK (active_generation > 0),
    claims_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    admission_open BOOLEAN NOT NULL DEFAULT FALSE,
    worker_owner TEXT,
    worker_heartbeat_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO deployment_release_control (singleton) VALUES (TRUE);
