-- Cartes de l'Opérateur sur les routes /admin (ADR 0016 §3 bis) : l'API fige
-- la requête proposée par l'agent ; la personne la valide auprès de l'API,
-- avec sa session ; l'Opérateur ne fait que relire l'issue. Il n'a jamais le
-- droit d'écrire.
CREATE TABLE IF NOT EXISTS operator_cards (
    id               uuid PRIMARY KEY,
    tool             varchar(64) NOT NULL,
    request          jsonb NOT NULL,
    target           text NOT NULL DEFAULT '',
    risk             varchar(8) NOT NULL CHECK (risk IN ('low', 'medium', 'high')),
    strong           boolean NOT NULL DEFAULT false,
    proposed_by      uuid NOT NULL,
    investigation_id uuid,
    justification    text NOT NULL DEFAULT '',
    status           varchar(12) NOT NULL DEFAULT 'proposed'
                     CHECK (status IN ('proposed', 'executing', 'executed', 'failed', 'rejected')),
    created_at       timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    decided_by       uuid,
    decided_email    text NOT NULL DEFAULT '',
    decided_at       timestamptz,
    http_status      int NOT NULL DEFAULT 0,
    message          text NOT NULL DEFAULT '',
    result           text NOT NULL DEFAULT '',
    executed_at      timestamptz
);
CREATE INDEX IF NOT EXISTS operator_cards_pending ON operator_cards (expires_at) WHERE status = 'proposed';
