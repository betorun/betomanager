-- Cartes d'action (ADR 0016 §6) : l'agent propose, une personne valide,
-- l'Opérateur exécute. Le passage proposée → validée est atomique (une
-- carte ne s'exécute qu'une fois).
CREATE TABLE cards (
    id               uuid PRIMARY KEY,
    investigation_id uuid NOT NULL REFERENCES investigations (id) ON DELETE CASCADE,
    kind             text NOT NULL,
    params           jsonb NOT NULL,
    target           text NOT NULL,
    summary          text NOT NULL,
    state            text NOT NULL DEFAULT '',
    rationale        text NOT NULL,
    risk             text NOT NULL CHECK (risk IN ('low', 'medium', 'high')),
    confirm          text NOT NULL DEFAULT '',
    status           text NOT NULL DEFAULT 'proposed'
                     CHECK (status IN ('proposed', 'approved', 'rejected', 'executed', 'failed', 'expired')),
    proposed_at      timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    decided_by       text NOT NULL DEFAULT '',
    decided_at       timestamptz,
    decision_note    text NOT NULL DEFAULT '',
    result           text NOT NULL DEFAULT '',
    executed_at      timestamptz
);
CREATE INDEX cards_investigation ON cards (investigation_id, proposed_at);

ALTER TABLE investigation_steps DROP CONSTRAINT investigation_steps_kind_check;
ALTER TABLE investigation_steps ADD CONSTRAINT investigation_steps_kind_check
    CHECK (kind IN ('question', 'answer', 'tool', 'action'));
