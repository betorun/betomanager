ALTER TABLE billing_accounts DROP COLUMN IF EXISTS plan_since;
ALTER TABLE billing_accounts DROP COLUMN IF EXISTS plan_id;
DROP TABLE IF EXISTS billing_plans;
