-- Services additionnels (ADR 0013) : une instance d'un modèle du catalogue
-- (redis, plus tard mariadb…) portée par un conteneur de sorte « service ».
-- Le mot de passe n'est PAS ici : il vit dans la variable secrète
-- ADDON_PASSWORD du conteneur porteur (chiffrée comme les autres).
CREATE TABLE IF NOT EXISTS project_addons (
    id          UUID PRIMARY KEY,
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Supprimer le conteneur porteur supprime l'instance.
    workload_id UUID NOT NULL UNIQUE REFERENCES workloads(id) ON DELETE CASCADE,
    name        VARCHAR(63) NOT NULL,
    template    VARCHAR(32) NOT NULL,
    version     VARCHAR(32) NOT NULL,
    mode        VARCHAR(32) NOT NULL DEFAULT '',
    size        VARCHAR(32) NOT NULL,
    disk        VARCHAR(16) NOT NULL DEFAULT '',
    created_by  VARCHAR(255) NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_project_addons_name ON project_addons(project_id, name);

-- Branchements : conteneurs qui ont reçu l'adresse (comme database_attachments).
CREATE TABLE IF NOT EXISTS addon_attachments (
    addon_id      UUID NOT NULL REFERENCES project_addons(id) ON DELETE CASCADE,
    workload_id   UUID NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
    workload_name VARCHAR(63) NOT NULL,
    variable      VARCHAR(128) NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (addon_id, workload_id, variable)
);

-- Plafond de services par projet (hors bases PostgreSQL), réglable par le staff.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS max_addons INT NOT NULL DEFAULT 10;
