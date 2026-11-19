-- Facturation, phase 4 (docs/adr/0012-facturation.md) : grâce et suspension
-- douce, reprise après recharge, factures et reçus, budgets par projet,
-- crédit d'essai une fois par numéro Mobile Money.

-- ── Grâce et suspension ─────────────────────────────────────────────────
-- grace_started_at : solde passé à zéro ou en dessous (début de la grâce).
-- suspended_at : conteneurs arrêtés faute de solde.
-- Informations de facturation (factures) : raison sociale, numéro fiscal,
-- adresse ; country existe déjà.
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS grace_started_at TIMESTAMPTZ;
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS suspended_at TIMESTAMPTZ;
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS legal_name TEXT NOT NULL DEFAULT '';
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS tax_id TEXT NOT NULL DEFAULT '';
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS address TEXT NOT NULL DEFAULT '';

-- Conteneurs arrêtés par la facturation (et seulement eux) : à la reprise,
-- ceux-là redémarrent, pas ceux que le client avait arrêtés lui-même.
-- reason : balance (solde épuisé) ou budget (budget mensuel du projet).
CREATE TABLE IF NOT EXISTS billing_stopped_workloads (
    workload_id     UUID        PRIMARY KEY,
    organization_id UUID        NOT NULL,
    project_id      UUID        NOT NULL,
    reason          VARCHAR(10) NOT NULL,
    stopped_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_billing_stopped_org ON billing_stopped_workloads (organization_id, reason);

-- ── Factures et reçus ───────────────────────────────────────────────────
-- Numérotation séquentielle sans trou, par préfixe et par année : le
-- compteur est pris sous verrou dans la transaction qui écrit le document.
CREATE TABLE IF NOT EXISTS billing_number_counters (
    prefix VARCHAR(10) NOT NULL,
    year   INTEGER     NOT NULL,
    last   INTEGER     NOT NULL DEFAULT 0,
    PRIMARY KEY (prefix, year)
);

-- Documents émis : immuables (comme le grand livre). PDF archivé tel
-- qu'émis. Montants en millièmes. kind : invoice (facture mensuelle) ou
-- receipt (reçu d'une recharge).
CREATE TABLE IF NOT EXISTS billing_documents (
    id              UUID PRIMARY KEY,
    organization_id UUID        NOT NULL,
    kind            VARCHAR(10) NOT NULL,
    number          TEXT        NOT NULL,
    period          VARCHAR(7)  NOT NULL DEFAULT '',
    topup_id        UUID,
    currency        VARCHAR(3)  NOT NULL,
    total           BIGINT      NOT NULL,
    vat             BIGINT      NOT NULL DEFAULT 0,
    vat_rate_bp     INTEGER     NOT NULL DEFAULT 0,
    data            JSONB       NOT NULL,
    pdf             BYTEA       NOT NULL,
    issued_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_billing_documents_number UNIQUE (number)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_documents_invoice ON billing_documents (organization_id, period) WHERE kind = 'invoice';
CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_documents_receipt ON billing_documents (topup_id) WHERE kind = 'receipt';
CREATE INDEX IF NOT EXISTS idx_billing_documents_org ON billing_documents (organization_id, issued_at DESC);

DROP TRIGGER IF EXISTS trg_billing_documents_append_only ON billing_documents;
CREATE TRIGGER trg_billing_documents_append_only BEFORE UPDATE OR DELETE ON billing_documents
    FOR EACH ROW EXECUTE FUNCTION billing_ledger_append_only();

-- ── Budgets par projet ──────────────────────────────────────────────────
-- monthly_limit : millièmes ; stop : arrêter les conteneurs du projet une
-- fois le budget atteint (sinon, alerte seulement). alert_level / period :
-- dernier seuil prévenu (1 : 80 %, 2 : 100 %) pour le mois period.
CREATE TABLE IF NOT EXISTS billing_project_budgets (
    project_id      UUID PRIMARY KEY,
    organization_id UUID        NOT NULL,
    monthly_limit   BIGINT      NOT NULL CHECK (monthly_limit > 0),
    stop            BOOLEAN     NOT NULL DEFAULT FALSE,
    alert_level     SMALLINT    NOT NULL DEFAULT 0,
    period          VARCHAR(7)  NOT NULL DEFAULT '',
    updated_by      TEXT        NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Crédit d'essai : une fois par numéro Mobile Money ───────────────────
-- Le paiement d'une recharge prouve que le numéro appartient au client :
-- le crédit d'essai est offert à la première recharge réussie, si ce numéro
-- n'en a jamais donné un.
CREATE TABLE IF NOT EXISTS billing_trials (
    phone           VARCHAR(20) PRIMARY KEY,
    organization_id UUID        NOT NULL UNIQUE,
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Grilles : durée de la grâce ─────────────────────────────────────────
INSERT INTO billing_catalogs (currency, version, data, created_by)
SELECT c.currency, c.version + 1, c.data || '{"grace_days": 3}'::jsonb, 'migration'
FROM (SELECT DISTINCT ON (currency) currency, version, data FROM billing_catalogs ORDER BY currency, version DESC) AS c
WHERE NOT (c.data ? 'grace_days');
