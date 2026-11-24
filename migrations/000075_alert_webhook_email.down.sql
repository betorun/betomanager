DELETE FROM alert_webhooks WHERE kind = 'email';
ALTER TABLE alert_webhooks DROP COLUMN IF EXISTS confirmed_at;
