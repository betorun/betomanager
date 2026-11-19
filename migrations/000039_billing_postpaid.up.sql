-- Facturation, phase 5 (docs/adr/0012-facturation.md) : postpayé entreprise.
-- L'usage est débité à l'heure comme en prépayé ; le solde devient une dette,
-- tolérée jusqu'au plafond de crédit. Chaque facture mensuelle est à payer
-- avant son échéance (virement ou mobile money) ; impayée : relance, puis
-- suspension douce. Activé par l'équipe, organisation par organisation.
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS postpaid BOOLEAN NOT NULL DEFAULT FALSE;
-- credit_limit : plafond de dette, en millièmes ; terms_days : échéance des
-- factures. NULL : valeurs par défaut de la grille.
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS credit_limit BIGINT;
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS terms_days INTEGER;

-- Échéance d'une facture (postpayé) ; NULL : facture déjà réglée par le solde.
ALTER TABLE billing_documents ADD COLUMN IF NOT EXISTS due_at TIMESTAMPTZ;

-- Relances : une par facture, puis la suspension (une fois).
CREATE TABLE IF NOT EXISTS billing_dunning (
    document_id  UUID PRIMARY KEY REFERENCES billing_documents(id),
    reminded_at  TIMESTAMPTZ,
    suspended_at TIMESTAMPTZ
);

-- Grilles : valeurs par défaut du postpayé.
INSERT INTO billing_catalogs (currency, version, data, created_by)
SELECT c.currency, c.version + 1,
       c.data || '{"postpaid_credit_limit": 200000, "postpaid_terms_days": 30, "postpaid_reminder_days": 7, "postpaid_suspend_days": 15}'::jsonb,
       'migration'
FROM (SELECT DISTINCT ON (currency) currency, version, data FROM billing_catalogs ORDER BY currency, version DESC) AS c
WHERE NOT (c.data ? 'postpaid_credit_limit');
