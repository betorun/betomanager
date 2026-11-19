-- IAM phase C (docs/architecture/iam.md §14) : politiques de refus.
-- Posées sur une organisation ou un projet ; évaluées avant les rôles, sur la
-- ressource et toutes ses descendantes. Un refus l'emporte toujours.
CREATE TABLE IF NOT EXISTS iam_deny_policies (
    id                   UUID PRIMARY KEY,
    resource_name        TEXT        NOT NULL,
    display_name         TEXT        NOT NULL,
    description          TEXT        NOT NULL DEFAULT '',
    -- Permissions exactes ou motifs ("service.*", "service.ressource.*", "*").
    denied_permissions   JSONB       NOT NULL,
    denied_principals    JSONB       NOT NULL,
    exception_principals JSONB       NOT NULL DEFAULT '[]',
    -- Fin de la politique (même format que la condition d'un binding).
    condition            JSONB,
    etag                 TEXT        NOT NULL,
    created_by           TEXT        NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by           TEXT        NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_iam_deny_policies_resource ON iam_deny_policies(resource_name);
