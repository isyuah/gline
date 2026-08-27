CREATE TABLE log_entries (
    id              bigserial PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    batch_id        uuid NOT NULL,
    batch_sequence  integer NOT NULL CHECK (batch_sequence >= 0),
    agent_id        uuid NOT NULL,
    pipeline_id     uuid NOT NULL,
    service         text NOT NULL CHECK (length(service) BETWEEN 1 AND 255),
    host            text NOT NULL CHECK (length(host) BETWEEN 1 AND 255),
    level           text NOT NULL CHECK (length(level) BETWEEN 1 AND 32),
    message         text NOT NULL,
    observed_at     timestamptz NOT NULL,
    ingested_at     timestamptz NOT NULL DEFAULT now(),
    attributes      jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(attributes) = 'object'),
    UNIQUE (project_id, batch_id, batch_sequence),
    FOREIGN KEY (project_id, batch_id)
        REFERENCES ingest_batches(project_id, id) ON DELETE CASCADE,
    FOREIGN KEY (project_id, agent_id, pipeline_id)
        REFERENCES pipelines(project_id, agent_id, id)
);

CREATE INDEX log_entries_project_time_idx
    ON log_entries (project_id, observed_at DESC, id DESC);
CREATE INDEX log_entries_project_service_time_idx
    ON log_entries (project_id, service, observed_at DESC, id DESC);
CREATE INDEX log_entries_project_level_time_idx
    ON log_entries (project_id, level, observed_at DESC, id DESC);
CREATE INDEX log_entries_project_ingested_idx
    ON log_entries (project_id, ingested_at, id);
CREATE INDEX log_entries_ingested_at_brin_idx
    ON log_entries USING brin (ingested_at);
