-- Support : tickets des clients, réponses de l'équipe (docs/modules/support.md).
CREATE TABLE IF NOT EXISTS support_tickets (
    id                  UUID PRIMARY KEY,
    organization_id     UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id          UUID,
    resource_kind       TEXT        NOT NULL DEFAULT '',
    resource_id         UUID,
    created_by          TEXT        NOT NULL DEFAULT '',
    created_by_user_id  UUID,
    created_by_email    TEXT        NOT NULL DEFAULT '',
    subject             TEXT        NOT NULL,
    description         TEXT        NOT NULL,
    status              TEXT        NOT NULL,
    priority            TEXT        NOT NULL,
    category            TEXT        NOT NULL DEFAULT '',
    tags                TEXT[]      NOT NULL DEFAULT '{}',
    assigned_to_user_id UUID,
    last_activity_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    closed_at           TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_support_tickets_org ON support_tickets(organization_id, last_activity_at DESC);
CREATE INDEX IF NOT EXISTS idx_support_tickets_status ON support_tickets(status, last_activity_at);

CREATE TABLE IF NOT EXISTS support_messages (
    id              UUID PRIMARY KEY,
    ticket_id       UUID        NOT NULL REFERENCES support_tickets(id) ON DELETE CASCADE,
    organization_id UUID        NOT NULL,
    author_user_id  UUID,
    author_role     TEXT        NOT NULL,
    body            TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_support_messages_ticket ON support_messages(ticket_id, created_at);

CREATE TABLE IF NOT EXISTS support_attachments (
    id         UUID PRIMARY KEY,
    ticket_id  UUID        NOT NULL REFERENCES support_tickets(id) ON DELETE CASCADE,
    message_id UUID REFERENCES support_messages(id) ON DELETE SET NULL,
    file_name  TEXT        NOT NULL,
    mime_type  TEXT        NOT NULL,
    size       BIGINT      NOT NULL,
    object_key TEXT        NOT NULL,
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_support_attachments_ticket ON support_attachments(ticket_id);

CREATE TABLE IF NOT EXISTS support_ticket_events (
    id              UUID PRIMARY KEY,
    ticket_id       UUID        NOT NULL REFERENCES support_tickets(id) ON DELETE CASCADE,
    organization_id UUID        NOT NULL,
    type            TEXT        NOT NULL,
    actor_user_id   UUID,
    payload         JSONB       NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_support_ticket_events_ticket ON support_ticket_events(ticket_id, created_at);

-- Réponses types de l'équipe (communes à toute la plateforme).
CREATE TABLE IF NOT EXISTS support_macros (
    id         UUID PRIMARY KEY,
    title      TEXT        NOT NULL,
    body       TEXT        NOT NULL,
    category   TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
