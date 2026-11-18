-- Accès d'un projet à un registre d'images externe (Docker Hub, ghcr.io…).
-- Le jeton est chiffré (AES-GCM, données associées liées à la ligne et à l'hôte)
-- et ne se relit jamais par l'API : il ne sert qu'au Secret de pull du projet.
CREATE TABLE IF NOT EXISTS project_registry_credentials (
    id              UUID PRIMARY KEY,
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    host            VARCHAR(253) NOT NULL,
    username        VARCHAR(255) NOT NULL,
    sealed_password BYTEA NOT NULL,
    created_by      UUID,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, host)
);
