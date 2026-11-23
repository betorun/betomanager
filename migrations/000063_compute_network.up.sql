-- VPS lot 7 : DNS inverse, blocage anti-spam, port 25, résiliation (docs/modules/compute.md).
ALTER TABLE compute_vps
    ADD COLUMN IF NOT EXISTS reverse_v4      TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reverse_v6      TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS spam_blocked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS smtp_allowed    BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS terminated_at   TIMESTAMPTZ;
