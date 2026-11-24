-- Dossiers d'investigation de l'Opérateur (ADR 0016 §5).
CREATE TABLE investigations (
    id            uuid PRIMARY KEY,
    subject_kind  text NOT NULL CHECK (subject_kind IN ('ticket', 'cluster', 'organization', 'incident', 'question')),
    subject_ref   text NOT NULL DEFAULT '',
    -- Motif : justification envoyée à chaque lecture de données d'un client.
    reason        text NOT NULL DEFAULT '',
    actor_user_id text NOT NULL,
    actor_email   text NOT NULL,
    model         text NOT NULL,
    status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'concluded', 'abandoned')),
    conclusion    text NOT NULL DEFAULT '',
    input_tokens  bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    concluded_at  timestamptz
);
CREATE INDEX investigations_recent ON investigations (created_at DESC, id);

CREATE TABLE investigation_steps (
    investigation_id uuid NOT NULL REFERENCES investigations (id) ON DELETE CASCADE,
    seq              int NOT NULL,
    kind             text NOT NULL CHECK (kind IN ('question', 'answer', 'tool')),
    tool             text NOT NULL DEFAULT '',
    input            jsonb,
    digest           text NOT NULL DEFAULT '',
    -- Résultat complet, effacé après la durée de conservation (le condensé reste).
    result           text,
    is_error         boolean NOT NULL DEFAULT false,
    duration_ms      int NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL,
    PRIMARY KEY (investigation_id, seq)
);
CREATE INDEX investigation_steps_purge ON investigation_steps (created_at) WHERE result IS NOT NULL;
