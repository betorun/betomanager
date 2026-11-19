-- Usage réel des disques (le stockage local-path ne plafonne pas la taille
-- demandée). Mesuré toutes les heures par un Job en lecture seule ; au-delà
-- de la taille payée : alertes (80 %, 100 %), puis arrêt du conteneur à
-- 120 % (données gardées). Une ligne par disque (PVC).
CREATE TABLE IF NOT EXISTS disk_usage (
    namespace      VARCHAR(63)  NOT NULL,
    pvc            VARCHAR(253) NOT NULL,
    project_id     UUID         NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- kind : workload (disque d'un conteneur, services compris) ou database.
    kind           VARCHAR(16)  NOT NULL,
    target_id      UUID         NOT NULL,
    target_name    VARCHAR(80)  NOT NULL,
    capacity_bytes BIGINT       NOT NULL,
    used_bytes     BIGINT       NOT NULL,
    -- level : 0, 80, 100 ou 120 (dernier seuil franchi).
    level          SMALLINT     NOT NULL DEFAULT 0,
    measured_at    TIMESTAMPTZ  NOT NULL,
    -- stopped_at : dernier arrêt pour disque plein (délai de grâce après une
    -- relance par le client).
    stopped_at     TIMESTAMPTZ,
    PRIMARY KEY (namespace, pvc)
);

CREATE INDEX IF NOT EXISTS idx_disk_usage_project ON disk_usage (project_id);
CREATE INDEX IF NOT EXISTS idx_disk_usage_target ON disk_usage (target_id);
