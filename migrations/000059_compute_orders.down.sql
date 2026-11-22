DROP INDEX IF EXISTS idx_compute_vps_due;
ALTER TABLE compute_vps
    DROP COLUMN IF EXISTS failure_code,
    DROP COLUMN IF EXISTS provider_order_id,
    DROP COLUMN IF EXISTS currency,
    DROP COLUMN IF EXISTS monthly_price;
