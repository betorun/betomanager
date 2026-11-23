-- Socle multi-cluster, lot 1 : registre des clusters (déclarés par l'équipe,
-- accès chiffrés) et leur santé relevée par le worker.

-- Un cluster peut être déclaré avant que son watcher ne s'enrôle.
ALTER TABLE clusters ALTER COLUMN enrolled_at DROP NOT NULL;
ALTER TABLE clusters ALTER COLUMN enrolled_at DROP DEFAULT;

ALTER TABLE clusters
    ADD COLUMN IF NOT EXISTS name                  TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS provider              TEXT        NOT NULL DEFAULT 'other',
    ADD COLUMN IF NOT EXISTS distribution          TEXT        NOT NULL DEFAULT 'k3s',
    ADD COLUMN IF NOT EXISTS region                TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS state                 TEXT        NOT NULL DEFAULT 'active',
    ADD COLUMN IF NOT EXISTS is_default            BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS notes                 TEXT        NOT NULL DEFAULT '',
    -- Kubeconfig chiffré (clé maître, AAD = cluster_id) ; NULL : le cluster
    -- par défaut lu dans la configuration du déploiement, ou pas encore d'accès.
    ADD COLUMN IF NOT EXISTS kubeconfig_ciphertext BYTEA,
    ADD COLUMN IF NOT EXISTS api_server            TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS kubeconfig_set_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS kubeconfig_set_by     TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS declared_by           TEXT        NOT NULL DEFAULT '';

ALTER TABLE clusters ADD CONSTRAINT clusters_provider_check CHECK (provider IN ('ovh', 'proxmox', 'other'));
ALTER TABLE clusters ADD CONSTRAINT clusters_distribution_check CHECK (distribution IN ('k3s', 'k8s'));
ALTER TABLE clusters ADD CONSTRAINT clusters_state_check CHECK (state IN ('active', 'closed', 'disabled'));
-- Un seul cluster par défaut.
CREATE UNIQUE INDEX IF NOT EXISTS uq_clusters_default ON clusters (is_default) WHERE is_default;

-- Dernier relevé de santé de chaque cluster (une ligne par cluster).
CREATE TABLE IF NOT EXISTS cluster_health (
    cluster_id        VARCHAR(100) PRIMARY KEY REFERENCES clusters(cluster_id) ON DELETE CASCADE,
    state             TEXT        NOT NULL,
    reasons           TEXT[]      NOT NULL DEFAULT '{}',
    error             TEXT        NOT NULL DEFAULT '',
    k8s_version       TEXT        NOT NULL DEFAULT '',
    latency_ms        INT         NOT NULL DEFAULT 0,
    nodes_total       INT         NOT NULL DEFAULT 0,
    nodes_ready       INT         NOT NULL DEFAULT 0,
    cpu_alloc_milli   BIGINT      NOT NULL DEFAULT 0,
    cpu_req_milli     BIGINT      NOT NULL DEFAULT 0,
    mem_alloc_bytes   BIGINT      NOT NULL DEFAULT 0,
    mem_req_bytes     BIGINT      NOT NULL DEFAULT 0,
    pods_running      INT         NOT NULL DEFAULT 0,
    pods_failing      INT         NOT NULL DEFAULT 0,
    checked_at        TIMESTAMPTZ NOT NULL,
    -- since : depuis quand le cluster est dans cet état (alerte au changement).
    since             TIMESTAMPTZ NOT NULL
);
