DROP INDEX IF EXISTS idx_projects_deletion_due;
ALTER TABLE projects DROP COLUMN IF EXISTS deletion_stopped;
ALTER TABLE projects DROP COLUMN IF EXISTS deletion_requested_by;
ALTER TABLE projects DROP COLUMN IF EXISTS deletion_scheduled_at;
