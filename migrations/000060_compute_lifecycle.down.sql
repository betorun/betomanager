DROP INDEX IF EXISTS uq_compute_requests_active_vps;
ALTER TABLE compute_vps
    DROP COLUMN IF EXISTS resume_state,
    DROP COLUMN IF EXISTS operation;
