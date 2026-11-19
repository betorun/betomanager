-- Imports de dumps dans les bases gérées : le dump (format personnalisé de
-- pg_dump) est gardé dans le stockage d'objets le temps qu'un Job du projet
-- le restaure avec l'utilisateur de la base (jamais un superutilisateur).
CREATE TABLE IF NOT EXISTS database_imports (
    id          UUID PRIMARY KEY,
    database_id UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    project_id  UUID NOT NULL,
    status      VARCHAR(16) NOT NULL,
    clean       BOOLEAN NOT NULL DEFAULT false,
    size_bytes  BIGINT NOT NULL,
    failure     TEXT NOT NULL DEFAULT '',
    created_by  VARCHAR(255) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_database_imports_database ON database_imports (database_id, created_at DESC);
