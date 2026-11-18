-- Taille des images construites et ménage du registre (quota par projet,
-- dernières images gardées par dépôt).
ALTER TABLE builds ADD COLUMN IF NOT EXISTS image_size BIGINT NOT NULL DEFAULT 0;
ALTER TABLE builds ADD COLUMN IF NOT EXISTS image_pruned_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_builds_project_images ON builds (project_id, name, finished_at DESC)
    WHERE status = 'SUCCEEDED' AND image_pruned_at IS NULL;
