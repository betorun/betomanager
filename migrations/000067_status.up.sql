-- Page de statut publique (docs/modules/status.md) : incidents et maintenances
-- annoncés par l'équipe, avec leur fil de messages. L'état des composants est
-- calculé par les sondes, rien n'est stocké.

CREATE TABLE IF NOT EXISTS status_incidents (
    id          UUID        PRIMARY KEY,
    title_fr    TEXT        NOT NULL,
    title_en    TEXT        NOT NULL,
    message_fr  TEXT        NOT NULL,
    message_en  TEXT        NOT NULL,
    -- clés des composants touchés : ["api", "vps", "domains"]
    components  JSONB       NOT NULL DEFAULT '[]',
    -- minor (dégradé) | major (panne) | maintenance
    impact      TEXT        NOT NULL CHECK (impact IN ('minor', 'major', 'maintenance')),
    status      TEXT        NOT NULL CHECK (status IN ('investigating', 'identified', 'monitoring', 'resolved')),
    started_at  TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    -- fenêtre d'une maintenance programmée
    starts_at   TIMESTAMPTZ,
    ends_at     TIMESTAMPTZ,
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by  TEXT        NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_status_incidents_open ON status_incidents (started_at DESC) WHERE status <> 'resolved';
CREATE INDEX IF NOT EXISTS idx_status_incidents_resolved ON status_incidents (resolved_at DESC) WHERE status = 'resolved';

CREATE TABLE IF NOT EXISTS status_incident_updates (
    id          UUID        PRIMARY KEY,
    incident_id UUID        NOT NULL REFERENCES status_incidents(id) ON DELETE CASCADE,
    status      TEXT        NOT NULL CHECK (status IN ('investigating', 'identified', 'monitoring', 'resolved')),
    message_fr  TEXT        NOT NULL,
    message_en  TEXT        NOT NULL,
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_status_incident_updates_incident ON status_incident_updates (incident_id, created_at);
