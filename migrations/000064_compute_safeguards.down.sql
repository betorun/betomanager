DROP TABLE IF EXISTS compute_provider_mails;
ALTER TABLE compute_vps DROP COLUMN IF EXISTS abuse_note;
ALTER TABLE compute_provider_vms DROP COLUMN IF EXISTS first_seen_at;
DROP TABLE IF EXISTS compute_org_limits;
DROP TABLE IF EXISTS compute_limits;
DROP TABLE IF EXISTS compute_stock;
