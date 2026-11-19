-- Multi-projets (Phase 2).
-- 1. Espace Kubernetes enregistré sur le projet : il ne dépend plus du nom.
--    Projets existants : leur espace actuel (km-<nom>) ; nouveaux : km-<id>.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS namespace VARCHAR(63);
UPDATE projects SET namespace = 'km-' || name WHERE namespace IS NULL;
ALTER TABLE projects ALTER COLUMN namespace SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_projects_namespace ON projects(namespace);

-- 2. Nom unique par organisation, projets supprimés exclus (avant : unique
--    sur toute la plateforme, suppressions comprises).
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS uq_projects_org_name
    ON projects(organization_id, lower(name)) WHERE deleted_at IS NULL;

-- 3. Nombre de projets par organisation (réglable par le staff).
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS max_projects INT NOT NULL DEFAULT 3;
