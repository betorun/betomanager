-- Socle multi-cluster, lot 2a : chaque projet porte son cluster, et les
-- réglages propres à un cluster (entrée HTTP, disques) vivent au registre.

ALTER TABLE clusters
    ADD COLUMN IF NOT EXISTS ingress_class     TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS ingress_namespace TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS ingress_target    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS storage_class     TEXT NOT NULL DEFAULT '';

-- NULL : cluster par défaut (projets créés hors placement, ex. reprise d'anciens workflows).
ALTER TABLE projects ADD COLUMN IF NOT EXISTS cluster_id VARCHAR(100)
    REFERENCES clusters(cluster_id) ON UPDATE CASCADE ON DELETE RESTRICT;
CREATE INDEX IF NOT EXISTS idx_projects_cluster ON projects (cluster_id) WHERE deleted_at IS NULL;

-- Projets existants : sur le cluster par défaut (le seul jusqu'ici).
UPDATE projects SET cluster_id = (SELECT cluster_id FROM clusters WHERE is_default LIMIT 1)
 WHERE cluster_id IS NULL;
