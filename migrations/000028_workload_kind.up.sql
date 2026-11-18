-- Sorte de conteneur : web (Service, ingress, sondes HTTP), worker (processus
-- continu sans port) ou cron (tâche planifiée). Les existants restent web.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS kind VARCHAR(10) NOT NULL DEFAULT 'web';
