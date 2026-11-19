DROP TABLE IF EXISTS billing_dunning;
ALTER TABLE billing_documents DROP COLUMN IF EXISTS due_at;
ALTER TABLE billing_accounts DROP COLUMN IF EXISTS terms_days;
ALTER TABLE billing_accounts DROP COLUMN IF EXISTS credit_limit;
ALTER TABLE billing_accounts DROP COLUMN IF EXISTS postpaid;
DELETE FROM billing_catalogs WHERE data ? 'postpaid_credit_limit' AND created_by = 'migration';
