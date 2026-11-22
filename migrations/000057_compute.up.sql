-- VPS revendus, option 1 (docs/modules/compute.md, tableau d'architecture).
-- Le monolithe garde l'état affiché au client ; le fournisseur et son
-- identifiant sont retenus sur chaque VPS, jamais exposés.

-- Offres : le prix de vente se calcule depuis le prix payé au fournisseur
-- (TTC si TVA) × taux de la grille (655,957 pour le FCFA) × coefficient,
-- arrondi au palier supérieur. Sans prix fournisseur : invendable.
CREATE TABLE IF NOT EXISTS compute_offers (
    code                 TEXT PRIMARY KEY,
    name                 TEXT        NOT NULL,
    provider_plan_code   TEXT        NOT NULL DEFAULT '',
    provider_price_cents BIGINT      NOT NULL DEFAULT 0 CHECK (provider_price_cents >= 0),
    coefficient_bp       BIGINT      NOT NULL CHECK (coefficient_bp > 0),
    round_to             BIGINT      NOT NULL CHECK (round_to > 0),
    specs                JSONB       NOT NULL DEFAULT '{}',
    locations            JSONB       NOT NULL DEFAULT '[]',
    images               JSONB       NOT NULL DEFAULT '{}',
    public               BOOLEAN     NOT NULL DEFAULT TRUE,
    position             INTEGER     NOT NULL DEFAULT 0,
    updated_by           TEXT        NOT NULL DEFAULT '',
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Décisions du 22/11 : VPS-1 et VPS-2, France (GRA, RBX, SBG), Ubuntu 24.04 et
-- Debian 12, coefficient ×2, palier de 500. Référence et prix fournisseur à
-- renseigner par l'équipe (/admin/compute/offers).
INSERT INTO compute_offers (code, name, coefficient_bp, round_to, locations, images, position) VALUES
    ('VPS-1', 'VPS-1', 20000, 500, '["gra","rbx","sbg"]', '{"ubuntu-24.04":"","debian-12":""}', 1),
    ('VPS-2', 'VPS-2', 20000, 500, '["gra","rbx","sbg"]', '{"ubuntu-24.04":"","debian-12":""}', 2)
ON CONFLICT (code) DO NOTHING;

CREATE TABLE IF NOT EXISTS compute_vps (
    id                  UUID PRIMARY KEY,
    organization_id     UUID        NOT NULL REFERENCES organizations(id),
    project_id          UUID        NOT NULL REFERENCES projects(id),
    name                TEXT        NOT NULL,
    offer               TEXT        NOT NULL REFERENCES compute_offers(code),
    location            TEXT        NOT NULL,
    image               TEXT        NOT NULL,
    provider            TEXT        NOT NULL,
    provider_id         TEXT        NOT NULL DEFAULT '',
    state               TEXT        NOT NULL,
    ipv4                TEXT        NOT NULL DEFAULT '',
    ipv6                TEXT        NOT NULL DEFAULT '',
    ssh_key_fingerprint TEXT        NOT NULL DEFAULT '',
    version             BIGINT      NOT NULL DEFAULT 1,
    paid_until          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Un nom par projet (hors résiliés) ; un identifiant fournisseur, une ligne.
CREATE UNIQUE INDEX IF NOT EXISTS uq_compute_vps_name ON compute_vps(project_id, name) WHERE state <> 'deleted';
CREATE UNIQUE INDEX IF NOT EXISTS uq_compute_vps_provider ON compute_vps(provider, provider_id) WHERE provider_id <> '';
CREATE INDEX IF NOT EXISTS idx_compute_vps_org ON compute_vps(organization_id);

-- Outbox : chaque opération est écrite ici dans la même transaction que la
-- commande ; un relais lance le workflow Temporal avec WorkflowID = id.
CREATE TABLE IF NOT EXISTS compute_requests (
    id              UUID PRIMARY KEY,
    organization_id UUID        NOT NULL,
    vps_id          UUID REFERENCES compute_vps(id),
    kind            TEXT        NOT NULL,
    payload         JSONB       NOT NULL DEFAULT '{}',
    status          TEXT        NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    last_error      TEXT        NOT NULL DEFAULT '',
    created_by      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_compute_requests_pending ON compute_requests(created_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_compute_requests_vps ON compute_requests(vps_id);

-- Secrets (mot de passe, mode secours) : chiffrés, lus une fois, expirés.
CREATE TABLE IF NOT EXISTS compute_secrets (
    id         UUID PRIMARY KEY,
    vps_id     UUID        NOT NULL REFERENCES compute_vps(id) ON DELETE CASCADE,
    kind       TEXT        NOT NULL,
    ciphertext BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    read_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_compute_secrets_expires ON compute_secrets(expires_at);
