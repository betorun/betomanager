-- IP de sortie dédiée par projet (ADR 0014, lot 1).

-- Passerelle de sortie du cluster ('' : aucune, 'cilium').
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS egress_gateway TEXT NOT NULL DEFAULT ''
    CHECK (egress_gateway IN ('', 'cilium'));

-- Réserve d'adresses de sortie : déclarées par l'équipe, attribuées à un projet.
CREATE TABLE IF NOT EXISTS egress_addresses (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    cluster_id   VARCHAR(100) NOT NULL REFERENCES clusters(cluster_id) ON UPDATE CASCADE ON DELETE RESTRICT,
    address      TEXT NOT NULL UNIQUE,
    gateway_node TEXT NOT NULL,
    project_id   UUID REFERENCES projects(id) ON DELETE SET NULL,
    assigned_at  TIMESTAMPTZ,
    assigned_by  TEXT NOT NULL DEFAULT '',
    notes        TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((project_id IS NULL) = (assigned_at IS NULL))
);

-- Une IP au plus par projet.
CREATE UNIQUE INDEX IF NOT EXISTS uq_egress_addresses_project ON egress_addresses(project_id) WHERE project_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_egress_addresses_free ON egress_addresses(cluster_id) WHERE project_id IS NULL;
