DROP TABLE IF EXISTS database_archives;
ALTER TABLE billing_accounts DROP COLUMN IF EXISTS data_purged_at;
ALTER TABLE billing_accounts DROP COLUMN IF EXISTS purge_noticed_at;
ALTER TABLE billing_accounts DROP COLUMN IF EXISTS purge_stage;
DROP VIEW IF EXISTS edge_suspended_projects;
DELETE FROM billing_catalogs WHERE data ? 'purge_days' AND created_by = 'migration';
