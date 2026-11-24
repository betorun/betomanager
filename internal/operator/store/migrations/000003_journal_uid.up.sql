-- UID de chaque entrée : clé d'idempotence de la recopie dans l'audit de la
-- plateforme (un id bigserial repartirait de 1 si la base était recréée).
ALTER TABLE journal ADD COLUMN uid uuid NOT NULL DEFAULT gen_random_uuid();
CREATE UNIQUE INDEX journal_uid ON journal (uid);
