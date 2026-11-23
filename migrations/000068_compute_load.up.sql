-- Tenue en charge du module compute (audit VPS, passe B).

-- Passe de réconciliation : VPS listés mais illisibles (relevé gardé).
ALTER TABLE compute_reconcile_runs ADD COLUMN IF NOT EXISTS unread INT NOT NULL DEFAULT 0;
-- Dernières passes (back-office) et passe en cours (une seule à la fois).
CREATE INDEX IF NOT EXISTS compute_reconcile_runs_open ON compute_reconcile_runs (provider, started_at) WHERE finished_at IS NULL;

-- Demandes lancées : relevé des demandes bloquées et purge.
CREATE INDEX IF NOT EXISTS idx_compute_requests_started ON compute_requests (started_at) WHERE status = 'started';
CREATE INDEX IF NOT EXISTS idx_compute_requests_closed ON compute_requests (finished_at) WHERE status IN ('done', 'failed');

-- E-mails du fournisseur : empreinte, un e-mail rangé deux fois ne fait qu'une ligne.
ALTER TABLE compute_provider_mails ADD COLUMN IF NOT EXISTS fingerprint TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS uq_compute_provider_mails_fingerprint ON compute_provider_mails (fingerprint) WHERE fingerprint <> '';
