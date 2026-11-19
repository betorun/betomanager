-- Plans (ADR 0012, phase 5) : le plan de l'organisation fixe les limites de
-- chacun de ses projets, et un rapprochement les y ramène (plan modifié,
-- nouveau projet). Une exception posée par l'équipe pour un projet (page
-- « Limites des projets ») est retenue ici : le rapprochement la respecte.
CREATE TABLE IF NOT EXISTS billing_limit_overrides (
    project_id    UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    cpu_limit     VARCHAR(20) NOT NULL,
    memory_limit  VARCHAR(20) NOT NULL,
    storage_limit VARCHAR(20) NOT NULL,
    set_by        TEXT        NOT NULL DEFAULT '',
    set_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
