DROP INDEX IF EXISTS idx_compute_secrets_vps;
ALTER TABLE compute_vps
    DROP COLUMN IF EXISTS rescue_requested_at,
    DROP COLUMN IF EXISTS rescue_secret,
    DROP COLUMN IF EXISTS rescue;
