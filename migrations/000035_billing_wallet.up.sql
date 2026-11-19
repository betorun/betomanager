-- Facturation, phase 3 (docs/adr/0012-facturation.md) : wallet par
-- organisation, grand livre en partie double, crédits offerts, recharges.

-- ── Grand livre ─────────────────────────────────────────────────────────
-- Une transaction regroupe des écritures dont la somme est nulle (partie
-- double). Rien ne se modifie ni ne se supprime : on corrige par une
-- transaction inverse. reference : clé d'idempotence (un débit horaire, une
-- recharge… ne s'écrit qu'une fois).
CREATE TABLE IF NOT EXISTS billing_transactions (
    id              UUID PRIMARY KEY,
    organization_id UUID,
    kind            VARCHAR(30) NOT NULL,
    reference       TEXT        NOT NULL,
    -- period : mois facturé (AAAA-MM) des débits d'usage.
    period          VARCHAR(7)  NOT NULL DEFAULT '',
    currency        VARCHAR(3)  NOT NULL,
    description     TEXT        NOT NULL DEFAULT '',
    created_by      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_billing_transactions_reference UNIQUE (reference)
);
CREATE INDEX IF NOT EXISTS idx_billing_transactions_org ON billing_transactions (organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_transactions_period ON billing_transactions (organization_id, kind, period);

-- Montants en millièmes d'unité de la monnaie, signés, jamais nuls.
-- Comptes : customer:<org>:cash, customer:<org>:credit (solde du client),
-- platform:revenue, platform:promotions, platform:xykpay, platform:adjustments.
CREATE TABLE IF NOT EXISTS billing_ledger_entries (
    id              BIGSERIAL PRIMARY KEY,
    transaction_id  UUID        NOT NULL REFERENCES billing_transactions(id),
    account         TEXT        NOT NULL,
    organization_id UUID,
    amount          BIGINT      NOT NULL CHECK (amount <> 0),
    currency        VARCHAR(3)  NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_billing_entries_account ON billing_ledger_entries (account);
CREATE INDEX IF NOT EXISTS idx_billing_entries_transaction ON billing_ledger_entries (transaction_id);

-- Partie double vérifiée par la base, à la validation de la transaction SQL :
-- une transaction déséquilibrée (ou en monnaies mêlées) fait échouer le COMMIT.
CREATE OR REPLACE FUNCTION billing_entries_balanced() RETURNS trigger AS $$
DECLARE
    total      BIGINT;
    currencies INTEGER;
BEGIN
    SELECT COALESCE(SUM(amount), 0), COUNT(DISTINCT currency) INTO total, currencies
    FROM billing_ledger_entries WHERE transaction_id = NEW.transaction_id;
    IF total <> 0 OR currencies > 1 THEN
        RAISE EXCEPTION 'billing transaction % is not balanced (sum %, currencies %)', NEW.transaction_id, total, currencies;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_billing_entries_balanced ON billing_ledger_entries;
CREATE CONSTRAINT TRIGGER trg_billing_entries_balanced
    AFTER INSERT ON billing_ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing_entries_balanced();

-- Immuables : ni modification, ni suppression, ni TRUNCATE.
CREATE OR REPLACE FUNCTION billing_ledger_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'the billing ledger is append-only: post a reversing transaction instead';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_billing_entries_append_only ON billing_ledger_entries;
CREATE TRIGGER trg_billing_entries_append_only BEFORE UPDATE OR DELETE ON billing_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION billing_ledger_append_only();
DROP TRIGGER IF EXISTS trg_billing_entries_no_truncate ON billing_ledger_entries;
CREATE TRIGGER trg_billing_entries_no_truncate BEFORE TRUNCATE ON billing_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION billing_ledger_append_only();
DROP TRIGGER IF EXISTS trg_billing_transactions_append_only ON billing_transactions;
CREATE TRIGGER trg_billing_transactions_append_only BEFORE UPDATE OR DELETE ON billing_transactions
    FOR EACH ROW EXECUTE FUNCTION billing_ledger_append_only();
DROP TRIGGER IF EXISTS trg_billing_transactions_no_truncate ON billing_transactions;
CREATE TRIGGER trg_billing_transactions_no_truncate BEFORE TRUNCATE ON billing_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION billing_ledger_append_only();

-- ── Crédits offerts ─────────────────────────────────────────────────────
-- Répartition du compte customer:<org>:credit par lot : consommés avant
-- l'argent payé, le plus proche de l'expiration d'abord. remaining évolue
-- avec les débits ; la somme des restes égale toujours le solde du compte.
CREATE TABLE IF NOT EXISTS billing_credit_grants (
    id              UUID PRIMARY KEY,
    organization_id UUID        NOT NULL,
    amount          BIGINT      NOT NULL CHECK (amount > 0),
    remaining       BIGINT      NOT NULL CHECK (remaining >= 0),
    currency        VARCHAR(3)  NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    reason          TEXT        NOT NULL DEFAULT '',
    transaction_id  UUID        NOT NULL REFERENCES billing_transactions(id),
    expired_at      TIMESTAMPTZ,
    created_by      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_billing_credit_grants_org ON billing_credit_grants (organization_id, expires_at) WHERE remaining > 0;

-- ── Recharges (mobile money par XYKPay) ─────────────────────────────────
CREATE TABLE IF NOT EXISTS billing_topups (
    id              UUID PRIMARY KEY,
    organization_id UUID        NOT NULL,
    amount          BIGINT      NOT NULL CHECK (amount > 0),
    currency        VARCHAR(3)  NOT NULL,
    phone           VARCHAR(20) NOT NULL,
    operator        VARCHAR(30) NOT NULL,
    -- pending | succeeded | failed | expired
    status          VARCHAR(20) NOT NULL,
    payment_id      TEXT,
    failure_code    TEXT        NOT NULL DEFAULT '',
    environment     VARCHAR(20) NOT NULL DEFAULT '',
    transaction_id  UUID REFERENCES billing_transactions(id),
    created_by      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at      TIMESTAMPTZ,
    CONSTRAINT uq_billing_topups_payment UNIQUE (payment_id)
);
CREATE INDEX IF NOT EXISTS idx_billing_topups_org ON billing_topups (organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_topups_pending ON billing_topups (created_at) WHERE status = 'pending';

-- ── Comptes ─────────────────────────────────────────────────────────────
-- charging : facturation réelle activée pour l'organisation (par l'équipe,
-- organisation par organisation) ; sans elle, le coût reste estimé.
-- alert_level : dernière alerte de solde envoyée (0 aucune, 1 sept jours,
-- 2 deux jours, 3 solde épuisé), remise à 0 quand le solde remonte.
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS charging BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS charging_since TIMESTAMPTZ;
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS alert_level SMALLINT NOT NULL DEFAULT 0;

-- ── Grilles : montants de la recharge et du crédit d'essai ──────────────
-- Nouvelle version de chaque grille, reprenant la précédente.
INSERT INTO billing_catalogs (currency, version, data, created_by)
SELECT c.currency, c.version + 1,
       c.data || '{"topup_min": 500, "topup_max": 500000, "trial_credit": 5000, "trial_days": 30}'::jsonb,
       'migration'
FROM (SELECT DISTINCT ON (currency) currency, version, data FROM billing_catalogs ORDER BY currency, version DESC) AS c
WHERE NOT (c.data ? 'topup_min');
