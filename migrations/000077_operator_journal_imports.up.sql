-- Recopie du journal de l'Opérateur dans l'audit (ADR 0016 §1) : chaque
-- entrée du serveur de gestion a un UID ; elle n'entre qu'une fois dans
-- audit_log, même si un lot est rejoué ou renvoyé après une coupure.
CREATE TABLE IF NOT EXISTS operator_journal_imports (
    uid         uuid PRIMARY KEY,
    audit_id    bigint,
    key_id      varchar(32) NOT NULL,
    imported_at timestamptz NOT NULL DEFAULT NOW()
);
