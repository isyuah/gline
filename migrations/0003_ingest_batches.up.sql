CREATE TABLE ingest_batches (
    id              uuid NOT NULL,
    project_id      uuid NOT NULL REFERENCES projects(id),
    agent_id        uuid NOT NULL,
    pipeline_id     uuid NOT NULL,
    sequence_no     bigint NOT NULL CHECK (sequence_no >= 0),
    payload_hash    bytea NOT NULL CHECK (octet_length(payload_hash) = 32),
    entry_count     integer NOT NULL CHECK (entry_count > 0),
    payload_bytes   integer NOT NULL CHECK (payload_bytes > 0),
    status          text NOT NULL CHECK (status IN ('committed', 'rejected', 'quarantined')),
    created_at      timestamptz NOT NULL,
    committed_at    timestamptz,
    error_code      text,
    PRIMARY KEY (project_id, id),
    FOREIGN KEY (project_id, agent_id, pipeline_id)
        REFERENCES pipelines(project_id, agent_id, id),
    CHECK ((status = 'committed') = (committed_at IS NOT NULL)),
    CHECK ((status = 'committed') = (error_code IS NULL))
);

CREATE INDEX ingest_batches_agent_created_idx
    ON ingest_batches (project_id, agent_id, created_at DESC);
