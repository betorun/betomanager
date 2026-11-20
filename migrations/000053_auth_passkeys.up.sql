-- Passkeys (WebAuthn) : second facteur, vérification avant une action
-- sensible, connexion sans mot de passe. La clé publique et le compteur de
-- signatures sont dans `credential` (JSON de go-webauthn).
CREATE TABLE IF NOT EXISTS auth_passkeys (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id BYTEA NOT NULL UNIQUE,
    credential    JSONB NOT NULL,
    name          VARCHAR(60) NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_auth_passkeys_user ON auth_passkeys (user_id, created_at);
