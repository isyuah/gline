CREATE TABLE agents (
    id                  uuid PRIMARY KEY,
    project_id          uuid NOT NULL REFERENCES projects(id),
    name                text NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
    hostname            text NOT NULL CHECK (length(hostname) BETWEEN 1 AND 255),
    version             text NOT NULL,
    status              text NOT NULL CHECK (status IN ('active', 'stale', 'disabled')),
    last_heartbeat_at   timestamptz,
    last_seen_ip        inet,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name),
    UNIQUE (project_id, id)
);

CREATE TABLE pipelines (
    id                  uuid PRIMARY KEY,
    project_id          uuid NOT NULL REFERENCES projects(id),
    agent_id            uuid NOT NULL,
    name                text NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
    service             text NOT NULL CHECK (length(service) BETWEEN 1 AND 255),
    config              jsonb NOT NULL CHECK (jsonb_typeof(config) = 'object'),
    config_version      bigint NOT NULL CHECK (config_version > 0),
    status              text NOT NULL CHECK (status IN ('enabled', 'paused', 'error', 'disabled')),
    reported_status     text NOT NULL DEFAULT 'stopped' CHECK (reported_status IN ('running', 'stopped', 'error')),
    reported_at         timestamptz,
    last_error          text,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, agent_id, name),
    UNIQUE (project_id, id),
    UNIQUE (project_id, agent_id, id),
    FOREIGN KEY (project_id, agent_id) REFERENCES agents(project_id, id)
);

CREATE TABLE api_keys (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    agent_id        uuid,
    name            text NOT NULL DEFAULT '' CHECK (length(name) <= 128),
    prefix          text NOT NULL CHECK (length(prefix) BETWEEN 8 AND 64),
    secret_hash     bytea NOT NULL CHECK (octet_length(secret_hash) >= 32),
    scopes          text[] NOT NULL CHECK (cardinality(scopes) > 0),
    status          text NOT NULL CHECK (status IN ('active', 'revoked', 'expired')),
    expires_at      timestamptz,
    last_used_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,
    UNIQUE (project_id, prefix),
    FOREIGN KEY (project_id, agent_id) REFERENCES agents(project_id, id),
    CHECK ((status = 'revoked') = (revoked_at IS NOT NULL)),
    CHECK (status <> 'expired' OR expires_at IS NOT NULL)
);

CREATE INDEX api_keys_active_prefix_idx ON api_keys (prefix) WHERE status = 'active';
CREATE INDEX agents_project_status_idx ON agents (project_id, status);
CREATE INDEX pipelines_project_status_idx ON pipelines (project_id, status);
