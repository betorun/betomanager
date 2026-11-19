-- Copie de sécurité prise avant de vider une base (pg_dump rangé dans le
-- stockage d'objets) : restaurable pendant 24 h, une par base.
CREATE TABLE IF NOT EXISTS database_snapshots (
    database_id UUID PRIMARY KEY REFERENCES databases(id) ON DELETE CASCADE,
    object_key  TEXT        NOT NULL,
    size_bytes  BIGINT      NOT NULL DEFAULT 0,
    taken_by    TEXT        NOT NULL DEFAULT '',
    taken_at    TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL
);
