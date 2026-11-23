ALTER TABLE compute_vps
    DROP COLUMN IF EXISTS terminated_at,
    DROP COLUMN IF EXISTS smtp_allowed,
    DROP COLUMN IF EXISTS spam_blocked_at,
    DROP COLUMN IF EXISTS reverse_v6,
    DROP COLUMN IF EXISTS reverse_v4;
