DROP INDEX IF EXISTS idx_projects_cluster;
ALTER TABLE projects DROP COLUMN IF EXISTS cluster_id;
ALTER TABLE clusters
    DROP COLUMN IF EXISTS storage_class,
    DROP COLUMN IF EXISTS ingress_target,
    DROP COLUMN IF EXISTS ingress_namespace,
    DROP COLUMN IF EXISTS ingress_class;
