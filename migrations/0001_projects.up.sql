CREATE TABLE projects (
    id          uuid PRIMARY KEY,
    slug        text NOT NULL UNIQUE CHECK (length(slug) BETWEEN 1 AND 63),
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
    status      text NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
