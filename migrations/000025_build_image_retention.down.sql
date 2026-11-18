DROP INDEX IF EXISTS idx_builds_project_images;
ALTER TABLE builds DROP COLUMN IF EXISTS image_pruned_at;
ALTER TABLE builds DROP COLUMN IF EXISTS image_size;
