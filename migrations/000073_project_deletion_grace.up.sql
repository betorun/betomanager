-- Délai de grâce avant l'effacement d'un projet (12 h) : la demande passe le
-- projet en PENDING_DELETION, arrête ses conteneurs et met ses bases en
-- veille ; il reste restaurable jusqu'à deletion_scheduled_at, puis la purge
-- lance la suppression définitive.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS deletion_scheduled_at TIMESTAMPTZ;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS deletion_requested_by TEXT;
-- deletion_stopped : conteneurs et bases arrêtés par la demande, relancés à
-- la restauration (ceux déjà arrêtés avant restent arrêtés).
ALTER TABLE projects ADD COLUMN IF NOT EXISTS deletion_stopped JSONB;
CREATE INDEX IF NOT EXISTS idx_projects_deletion_due ON projects(deletion_scheduled_at)
    WHERE deletion_scheduled_at IS NOT NULL;
