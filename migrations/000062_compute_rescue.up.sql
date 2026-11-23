-- VPS lot 6 : mode secours ; mot de passe intercepté, chiffré, affiché une fois.
ALTER TABLE compute_vps
    ADD COLUMN IF NOT EXISTS rescue              TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS rescue_secret       TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS rescue_requested_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_compute_secrets_vps ON compute_secrets (vps_id, kind);
