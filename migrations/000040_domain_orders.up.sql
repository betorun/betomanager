-- Revente de noms de domaine (ADR 0012, phase 5). Le client achète depuis la
-- console, payé par son solde ; betorun commande chez le registraire (OVH),
-- le client en est le titulaire. Prix en monnaie du compte : prix du
-- registraire converti, marge et arrondi de la grille.
CREATE TABLE IF NOT EXISTS domain_orders (
    id              UUID PRIMARY KEY,
    organization_id UUID        NOT NULL,
    domain          TEXT        NOT NULL,
    -- ordering (payé, à commander) | placed (commandé, en création) |
    -- active | failed (remboursé) | expired
    status          VARCHAR(20) NOT NULL,
    currency        VARCHAR(3)  NOT NULL,
    -- price / renew_price : payé par le client (millièmes) ; *_cents : prix
    -- du registraire (sa devise), pour contrôle et marge.
    price           BIGINT      NOT NULL,
    renew_price     BIGINT      NOT NULL,
    registrar_cents BIGINT      NOT NULL DEFAULT 0,
    renew_cents     BIGINT      NOT NULL DEFAULT 0,
    registrar_currency VARCHAR(3) NOT NULL DEFAULT '',
    contact         JSONB       NOT NULL,
    registrar_order TEXT        NOT NULL DEFAULT '',
    auto_renew      BOOLEAN     NOT NULL DEFAULT TRUE,
    -- paid_until : fin de la période déjà payée par le client (achat, puis
    -- chaque renouvellement débité).
    expires_at      TIMESTAMPTZ,
    paid_until      TIMESTAMPTZ,
    renew_failed_at TIMESTAMPTZ,
    error           TEXT        NOT NULL DEFAULT '',
    created_by      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    active_at       TIMESTAMPTZ
);
-- Un domaine n'est détenu qu'une fois (une commande ratée le libère).
CREATE UNIQUE INDEX IF NOT EXISTS uq_domain_orders_domain ON domain_orders (domain) WHERE status NOT IN ('failed', 'expired');
CREATE INDEX IF NOT EXISTS idx_domain_orders_org ON domain_orders (organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_domain_orders_status ON domain_orders (status);

-- Grilles : prix des domaines (taux de conversion figé du FCFA, marge, arrondi).
INSERT INTO billing_catalogs (currency, version, data, created_by)
SELECT c.currency, c.version + 1,
       c.data || '{"domain_margin_bp": 2000, "domain_round_to": 500, "eur_rate_milli": 655957}'::jsonb,
       'migration'
FROM (SELECT DISTINCT ON (currency) currency, version, data FROM billing_catalogs ORDER BY currency, version DESC) AS c
WHERE NOT (c.data ? 'domain_margin_bp');
