-- Accès d'assistance (ADR 0017) : une personne nommée de l'équipe betorun
-- reçoit, pour un temps borné, le rôle roles/support.assistant sur une
-- organisation, accordé par le client ou ouvert en bris de glace. La table est
-- la source de vérité ; la liaison IAM en découle.
CREATE TABLE IF NOT EXISTS assistance_grants (
    id              uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    ticket_id       uuid REFERENCES support_tickets (id) ON DELETE SET NULL,
    staff_user_id   uuid NOT NULL,
    staff_email     text NOT NULL DEFAULT '',
    mode            varchar(12) NOT NULL CHECK (mode IN ('consent', 'breakglass')),
    status          varchar(10) NOT NULL
                    CHECK (status IN ('requested', 'active', 'denied', 'revoked', 'expired', 'closed')),
    reason          text NOT NULL,
    requested_hours int NOT NULL CHECK (requested_hours BETWEEN 1 AND 24),
    requested_at    timestamptz NOT NULL,
    decided_by      uuid,
    decided_email   text NOT NULL DEFAULT '',
    decided_at      timestamptz,
    expires_at      timestamptz,
    ended_at        timestamptz,
    ended_by        text NOT NULL DEFAULT '',
    end_note        text NOT NULL DEFAULT ''
);
-- Une demande ou un accès ouvert à la fois par personne et par organisation.
CREATE UNIQUE INDEX IF NOT EXISTS assistance_grants_open
    ON assistance_grants (organization_id, staff_user_id) WHERE status IN ('requested', 'active');
CREATE INDEX IF NOT EXISTS assistance_grants_org ON assistance_grants (organization_id, requested_at DESC);
CREATE INDEX IF NOT EXISTS assistance_grants_active ON assistance_grants (expires_at) WHERE status = 'active';
