-- VPS lot 2 : relevé du fournisseur et passes de réconciliation (docs/modules/compute.md).

-- Dernier relevé de chaque VPS chez le fournisseur, remplacé à chaque passe.
CREATE TABLE IF NOT EXISTS compute_provider_vms (
    provider     TEXT        NOT NULL,
    provider_id  TEXT        NOT NULL,
    vps_id       UUID        REFERENCES compute_vps(id) ON DELETE SET NULL,
    state        TEXT        NOT NULL,
    raw_state    TEXT        NOT NULL DEFAULT '',
    plan         TEXT        NOT NULL DEFAULT '',
    zone         TEXT        NOT NULL DEFAULT '',
    ipv4         TEXT        NOT NULL DEFAULT '',
    ipv6         TEXT        NOT NULL DEFAULT '',
    busy         BOOLEAN     NOT NULL DEFAULT FALSE,
    drift        TEXT        NOT NULL DEFAULT '',
    seen_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (provider, provider_id)
);

CREATE TABLE IF NOT EXISTS compute_reconcile_runs (
    id           UUID        PRIMARY KEY,
    provider     TEXT        NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL,
    finished_at  TIMESTAMPTZ,
    seen         INT         NOT NULL DEFAULT 0,
    updated      INT         NOT NULL DEFAULT 0,
    orphans      INT         NOT NULL DEFAULT 0,
    missing      INT         NOT NULL DEFAULT 0,
    drifts       INT         NOT NULL DEFAULT 0,
    error        TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS compute_reconcile_runs_started ON compute_reconcile_runs (started_at DESC);
