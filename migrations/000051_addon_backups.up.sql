-- Sauvegardes des services qui en ont (MariaDB) : une copie chaque nuit,
-- rangée dans le stockage d'objets ; les 7 dernières réussies sont gardées.
CREATE TABLE IF NOT EXISTS addon_backups (
    id          UUID PRIMARY KEY,
    addon_id    UUID NOT NULL REFERENCES project_addons(id) ON DELETE CASCADE,
    project_id  UUID NOT NULL,
    status      VARCHAR(16) NOT NULL,             -- running, completed, failed
    trigger     VARCHAR(16) NOT NULL,             -- nightly, manual, before_restore
    object_key  TEXT NOT NULL,
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    restored_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS addon_backups_addon_idx ON addon_backups (addon_id, created_at DESC);
