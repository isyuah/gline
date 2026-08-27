CREATE TABLE quarantine_batches (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    batch_id        uuid NOT NULL,
    payload         bytea NOT NULL CHECK (octet_length(payload) > 0),
    payload_hash    bytea NOT NULL CHECK (octet_length(payload_hash) = 32),
    error_code      text NOT NULL,
    error_detail    text NOT NULL,
    status          text NOT NULL CHECK (status IN ('pending', 'replaying', 'resolved', 'discarded')),
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    claimed_at      timestamptz,
    resolved_at     timestamptz,
    UNIQUE (project_id, batch_id),
    CHECK (
        (status = 'pending' AND claimed_at IS NULL AND resolved_at IS NULL) OR
        (status = 'replaying' AND claimed_at IS NOT NULL AND resolved_at IS NULL) OR
        (status IN ('resolved', 'discarded') AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX quarantine_pending_idx
    ON quarantine_batches (created_at, id) WHERE status = 'pending';
CREATE INDEX quarantine_replaying_lease_idx
    ON quarantine_batches (claimed_at) WHERE status = 'replaying';

CREATE TABLE retention_policies (
    project_id      uuid PRIMARY KEY REFERENCES projects(id),
    max_age_seconds bigint NOT NULL CHECK (max_age_seconds > 0),
    max_bytes       bigint CHECK (max_bytes IS NULL OR max_bytes > 0),
    enabled         boolean NOT NULL DEFAULT true,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE usage_buckets (
    project_id      uuid NOT NULL REFERENCES projects(id),
    bucket_start    timestamptz NOT NULL,
    entries         bigint NOT NULL DEFAULT 0 CHECK (entries >= 0),
    bytes           bigint NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    failed_batches  bigint NOT NULL DEFAULT 0 CHECK (failed_batches >= 0),
    PRIMARY KEY (project_id, bucket_start),
    CHECK (bucket_start = date_trunc('minute', bucket_start))
);

CREATE TABLE audit_events (
    id              bigserial PRIMARY KEY,
    project_id      uuid REFERENCES projects(id),
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    action          text NOT NULL,
    resource        text NOT NULL,
    resource_id     text NOT NULL,
    outcome         text NOT NULL CHECK (outcome IN ('success', 'rejected', 'failed')),
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_project_time_idx
    ON audit_events (project_id, created_at DESC, id DESC);
