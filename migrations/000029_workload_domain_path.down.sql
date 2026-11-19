-- Retour à un nom = un conteneur : les routes à chemin sont supprimées.
DELETE FROM workload_domains WHERE path <> '/';

DROP INDEX IF EXISTS uq_workload_domains_verified;
CREATE UNIQUE INDEX IF NOT EXISTS uq_workload_domains_verified ON workload_domains (hostname) WHERE verified;

ALTER TABLE workload_domains DROP CONSTRAINT IF EXISTS workload_domains_workload_id_hostname_path_key;
ALTER TABLE workload_domains ADD CONSTRAINT workload_domains_workload_id_hostname_key UNIQUE (workload_id, hostname);

ALTER TABLE workload_domains DROP COLUMN IF EXISTS path;
