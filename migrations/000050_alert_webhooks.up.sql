-- Webhooks d'alertes d'un projet (Slack ou JSON signé) : mêmes alertes que
-- les e-mails aux propriétaires et éditeurs. L'URL (souvent secrète, ex.
-- Slack) et le secret de signature sont chiffrés.
CREATE TABLE alert_webhooks (
    id               UUID PRIMARY KEY,
    project_id       UUID         NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name             VARCHAR(60)  NOT NULL,
    kind             VARCHAR(10)  NOT NULL,
    url_sealed       BYTEA        NOT NULL,
    url_hint         VARCHAR(120) NOT NULL,
    secret_sealed    BYTEA,
    locale           VARCHAR(5)   NOT NULL DEFAULT 'fr',
    created_by       VARCHAR(80)  NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_delivery_at TIMESTAMPTZ,
    last_status      VARCHAR(60)  NOT NULL DEFAULT ''
);
CREATE INDEX idx_alert_webhooks_project ON alert_webhooks (project_id, created_at);
