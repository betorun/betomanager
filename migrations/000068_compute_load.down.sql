DROP INDEX IF EXISTS uq_compute_provider_mails_fingerprint;
ALTER TABLE compute_provider_mails DROP COLUMN IF EXISTS fingerprint;
DROP INDEX IF EXISTS idx_compute_requests_closed;
DROP INDEX IF EXISTS idx_compute_requests_started;
DROP INDEX IF EXISTS compute_reconcile_runs_open;
ALTER TABLE compute_reconcile_runs DROP COLUMN IF EXISTS unread;
