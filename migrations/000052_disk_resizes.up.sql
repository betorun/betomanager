-- Agrandissements de disque par copie (module diskresize).
CREATE TABLE IF NOT EXISTS disk_resizes (
    id          UUID PRIMARY KEY,
    workload_id UUID NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
    project_id  UUID NOT NULL,
    from_size   VARCHAR(20) NOT NULL,
    to_size     VARCHAR(20) NOT NULL,
    phase       VARCHAR(20) NOT NULL,
    status      VARCHAR(20) NOT NULL,
    error       TEXT NOT NULL DEFAULT '',
    was_stopped BOOLEAN NOT NULL DEFAULT FALSE,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_disk_resizes_workload ON disk_resizes (workload_id, created_at DESC);
