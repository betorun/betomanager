-- Facturation (docs/adr/0012-facturation.md) : organisation suspendue.
--
-- 1. Sites : l'edge (static-edge, rôle edge_ro en lecture seule) sert une page
--    neutre « temporairement indisponible » pour les sites d'une organisation
--    suspendue. Il ne lit que cette vue : des identifiants de projet, aucun
--    solde ni montant.
CREATE OR REPLACE VIEW edge_suspended_projects AS
SELECT p.id AS project_id
FROM projects p
JOIN billing_accounts a ON a.organization_id = p.organization_id
WHERE a.charging AND a.suspended_at IS NOT NULL;

-- 2. Suppression des données après purge_days jours de suspension.
-- purge_stage : dernier avertissement envoyé (1 : J+7, 2 : une semaine avant,
-- 3 : deux jours avant, 4 : suppression faite ou en cours).
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS purge_stage SMALLINT NOT NULL DEFAULT 0;
-- purge_noticed_at : envoi de ce dernier avertissement (la suppression attend
-- au moins deux jours après le dernier).
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS purge_noticed_at TIMESTAMPTZ;
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS data_purged_at TIMESTAMPTZ;

-- Copies finales des bases supprimées (pg_dump dans le stockage d'objets),
-- restaurables par l'équipe jusqu'à expires_at. Pas de clé étrangère : la
-- base n'existe plus.
CREATE TABLE IF NOT EXISTS database_archives (
    id              UUID PRIMARY KEY,
    organization_id UUID        NOT NULL,
    project_id      UUID        NOT NULL,
    database_id     UUID        NOT NULL,
    database_name   TEXT        NOT NULL,
    object_key      TEXT        NOT NULL,
    size_bytes      BIGINT      NOT NULL DEFAULT 0,
    reason          TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_database_archives_org ON database_archives(organization_id);
CREATE INDEX IF NOT EXISTS idx_database_archives_expires ON database_archives(expires_at);

-- Grilles : 30 jours de suspension avant la suppression.
INSERT INTO billing_catalogs (currency, version, data, created_by)
SELECT c.currency, c.version + 1, c.data || '{"purge_days": 30}'::jsonb, 'migration'
FROM (SELECT DISTINCT ON (currency) currency, version, data FROM billing_catalogs ORDER BY currency, version DESC) AS c
WHERE NOT (c.data ? 'purge_days');
