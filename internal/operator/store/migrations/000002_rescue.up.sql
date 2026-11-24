-- Accès de secours de l'équipe (ADR 0016 §1) : comptes locaux nominatifs,
-- mot de passe et TOTP obligatoires ; utilisables quand l'API ou l'annuaire
-- ne répondent plus. Limités au chemin de secours (jamais les données des
-- clients).
CREATE TABLE rescue_accounts (
    id              uuid PRIMARY KEY,
    email           text NOT NULL UNIQUE,
    password_hash   text NOT NULL,
    -- Secret TOTP chiffré (AES-256-GCM, clé hors base, lié au compte).
    totp_secret     bytea NOT NULL,
    totp_last_step  bigint NOT NULL DEFAULT 0,
    failed_attempts int NOT NULL DEFAULT 0,
    locked_until    timestamptz,
    disabled_at     timestamptz,
    created_at      timestamptz NOT NULL,
    last_login_at   timestamptz
);

CREATE TABLE rescue_sessions (
    token_hash bytea PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES rescue_accounts (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

-- Journal du serveur de gestion : connexions de secours, dossiers ouverts,
-- questions, clôtures. Recopié dans l'audit de la plateforme au retour de
-- l'API (synced_at).
CREATE TABLE journal (
    id               bigserial PRIMARY KEY,
    at               timestamptz NOT NULL,
    actor            text NOT NULL,
    kind             text NOT NULL,
    investigation_id uuid,
    detail           jsonb NOT NULL DEFAULT '{}',
    synced_at        timestamptz
);
CREATE INDEX journal_unsynced ON journal (id) WHERE synced_at IS NULL;
