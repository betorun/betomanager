-- Chemin d'un domaine personnalisé : un même nom peut servir plusieurs
-- conteneurs d'un projet (ex. /auth vers Keycloak, le reste vers le site).
-- Unicité : une route (nom + chemin) vérifiée n'a qu'un conteneur.
ALTER TABLE workload_domains ADD COLUMN IF NOT EXISTS path VARCHAR(100) NOT NULL DEFAULT '/';

ALTER TABLE workload_domains DROP CONSTRAINT IF EXISTS workload_domains_workload_id_hostname_key;
ALTER TABLE workload_domains ADD CONSTRAINT workload_domains_workload_id_hostname_path_key UNIQUE (workload_id, hostname, path);

DROP INDEX IF EXISTS uq_workload_domains_verified;
CREATE UNIQUE INDEX IF NOT EXISTS uq_workload_domains_verified ON workload_domains (hostname, path) WHERE verified;
