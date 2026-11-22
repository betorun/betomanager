DROP TABLE IF EXISTS compute_vps_backups;
ALTER TABLE compute_vps DROP COLUMN IF EXISTS snapshot_option;
ALTER TABLE compute_offers
    DROP COLUMN IF EXISTS snapshot_price_cents,
    DROP COLUMN IF EXISTS snapshot_plan_code;
