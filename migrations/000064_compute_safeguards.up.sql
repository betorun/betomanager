-- VPS lot 8 : stock du fournisseur, quotas et anti-fraude, procédure abus,
-- e-mails du fournisseur pour l'équipe (docs/modules/compute.md).

-- Stock par offre et emplacement, relevé par le mode compute.
CREATE TABLE IF NOT EXISTS compute_stock (
    offer        TEXT        NOT NULL REFERENCES compute_offers(code) ON DELETE CASCADE,
    location     TEXT        NOT NULL,
    status       TEXT        NOT NULL,            -- available, out_of_stock, preorder
    days         INT         NOT NULL DEFAULT 0,  -- délai de livraison annoncé
    checked_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (offer, location)
);

-- Réglages de la plateforme (une ligne) : quotas et limites des comptes récents.
CREATE TABLE IF NOT EXISTS compute_limits (
    id                   SMALLINT    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    max_vps_per_org      INT         NOT NULL,
    new_account_days     INT         NOT NULL,
    new_account_max_vps  INT         NOT NULL,
    new_account_offers   JSONB       NOT NULL,
    updated_by           TEXT        NOT NULL DEFAULT '',
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO compute_limits (max_vps_per_org, new_account_days, new_account_max_vps, new_account_offers)
VALUES (5, 30, 1, '["VPS-1"]') ON CONFLICT (id) DO NOTHING;

-- Plafond propre à une organisation (accordé par l'équipe), sinon celui de la plateforme.
CREATE TABLE IF NOT EXISTS compute_org_limits (
    organization_id UUID        PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    max_vps         INT         NOT NULL,
    trusted         BOOLEAN     NOT NULL DEFAULT FALSE, -- exempté des limites des comptes récents
    updated_by      TEXT        NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Première apparition d'un VPS chez le fournisseur (rattachement d'un VPS livré).
ALTER TABLE compute_provider_vms ADD COLUMN IF NOT EXISTS first_seen_at TIMESTAMPTZ;

-- Abus : motif d'une suspension par l'équipe.
ALTER TABLE compute_vps ADD COLUMN IF NOT EXISTS abuse_note TEXT NOT NULL DEFAULT '';

-- E-mails du fournisseur (incidents, abus…) reçus par la boîte technique, pour l'équipe.
CREATE TABLE IF NOT EXISTS compute_provider_mails (
    id          UUID        PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL,
    sender      TEXT        NOT NULL DEFAULT '',
    subject     TEXT        NOT NULL DEFAULT '',
    body        TEXT        NOT NULL DEFAULT '',
    kind        TEXT        NOT NULL DEFAULT 'other', -- abuse, incident, other
    vps_id      UUID        REFERENCES compute_vps(id) ON DELETE SET NULL,
    handled_at  TIMESTAMPTZ,
    handled_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_compute_provider_mails_open ON compute_provider_mails (received_at DESC) WHERE handled_at IS NULL;
