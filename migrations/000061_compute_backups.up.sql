-- VPS lot 5 : sauvegardes automatiques et option snapshot (docs/modules/compute.md).
ALTER TABLE compute_offers
    ADD COLUMN IF NOT EXISTS snapshot_plan_code   TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS snapshot_price_cents BIGINT NOT NULL DEFAULT 0;

ALTER TABLE compute_vps
    ADD COLUMN IF NOT EXISTS snapshot_option TEXT NOT NULL DEFAULT '';

-- Relevé des sauvegardes par le mode compute (l'API n'a pas les clés du fournisseur).
CREATE TABLE IF NOT EXISTS compute_vps_backups (
    vps_id          UUID        PRIMARY KEY REFERENCES compute_vps(id) ON DELETE CASCADE,
    automated       BOOLEAN     NOT NULL DEFAULT FALSE,
    restore_points  JSONB       NOT NULL DEFAULT '[]',
    mounted         JSONB       NOT NULL DEFAULT '[]',
    snapshot        JSONB,
    refreshed_at    TIMESTAMPTZ NOT NULL
);
