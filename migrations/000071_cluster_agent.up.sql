-- Socle multi-cluster, lot 3 : agent (watcher) installé par la plateforme.
-- agent_key_id : fichier de clé en service (révoqué quand un nouveau le remplace).
ALTER TABLE clusters
    ADD COLUMN IF NOT EXISTS agent_image        TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS agent_key_id       TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS agent_installed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS agent_installed_by TEXT NOT NULL DEFAULT '';
