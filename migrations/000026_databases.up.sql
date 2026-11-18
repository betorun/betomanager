-- Bases de données gérées (Postgres via CloudNativePG), une par ligne.
CREATE TABLE IF NOT EXISTS databases (
    id           UUID PRIMARY KEY,
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name         VARCHAR(30) NOT NULL,
    engine       VARCHAR(20) NOT NULL DEFAULT 'postgres',
    storage_size VARCHAR(10) NOT NULL,
    status       VARCHAR(20) NOT NULL,
    created_by   VARCHAR(80) NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT databases_project_name UNIQUE (project_id, name)
);
CREATE INDEX IF NOT EXISTS idx_databases_project ON databases (project_id, created_at DESC);
