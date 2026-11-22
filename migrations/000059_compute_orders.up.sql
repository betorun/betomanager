-- VPS lot 3 : commande payée par le solde, renouvellement mensuel (docs/modules/compute.md).
ALTER TABLE compute_vps
    ADD COLUMN IF NOT EXISTS monthly_price      BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS currency           TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS provider_order_id  TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS failure_code       TEXT   NOT NULL DEFAULT '';

-- Renouvellements dus (worker).
CREATE INDEX IF NOT EXISTS idx_compute_vps_due ON compute_vps (paid_until)
    WHERE state IN ('running', 'stopped', 'suspended');
