-- Stockage objet S3 par projet (ADR 0015) : buckets aux noms S3 générés et
-- clés d'accès, chacune une identité S3 limitée à un seul bucket.
CREATE TABLE IF NOT EXISTS storage_buckets (
    id          UUID PRIMARY KEY,
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    bucket_name TEXT NOT NULL UNIQUE,
    quota_bytes BIGINT NOT NULL,
    used_bytes  BIGINT NOT NULL DEFAULT 0,
    usage_at    TIMESTAMPTZ,
    over_quota  BOOLEAN NOT NULL DEFAULT FALSE,
    -- public : lecture publique des objets ; suspended : projet en délai de
    -- grâce (clés désactivées, données gardées).
    public      BOOLEAN NOT NULL DEFAULT FALSE,
    suspended   BOOLEAN NOT NULL DEFAULT FALSE,
    backend     TEXT NOT NULL,
    status      TEXT NOT NULL,
    created_by  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, name)
);

CREATE TABLE IF NOT EXISTS storage_keys (
    id            UUID PRIMARY KEY,
    bucket_id     UUID NOT NULL REFERENCES storage_buckets(id) ON DELETE CASCADE,
    project_id    UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    access_key_id TEXT NOT NULL UNIQUE,
    access        TEXT NOT NULL,
    kind          TEXT NOT NULL,
    workload_id   UUID,
    prefix        TEXT,
    created_by    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rotated_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_storage_keys_bucket ON storage_keys(bucket_id);
-- Un seul branchement par couple (bucket, conteneur).
CREATE UNIQUE INDEX IF NOT EXISTS idx_storage_keys_attachment ON storage_keys(bucket_id, workload_id)
    WHERE workload_id IS NOT NULL;
