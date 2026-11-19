-- Branchements d'une base : quel conteneur la reçoit, sous quelle variable.
-- La carte de la base les affiche, et l'avertissement d'un import qui
-- remplace les données les nomme.
CREATE TABLE IF NOT EXISTS database_attachments (
    database_id   UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    workload_id   UUID NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
    workload_name VARCHAR(63) NOT NULL,
    variable      VARCHAR(128) NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (database_id, workload_id, variable)
);
