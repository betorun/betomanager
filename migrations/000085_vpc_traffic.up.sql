-- Trafic du réseau privé (ADR 0019) : mesuré pour l'équipe avant de décider
-- d'un prix ; rien n'est facturé.

-- Derniers compteurs lus pour chaque VPS (ils repartent de zéro quand le
-- lien est refait : un relevé plus bas qu'avant compte depuis zéro).
CREATE TABLE IF NOT EXISTS vpc_traffic_peers (
    vps_id          UUID PRIMARY KEY REFERENCES compute_vps(id) ON DELETE CASCADE,
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    from_vps_bytes  BIGINT NOT NULL DEFAULT 0,
    to_vps_bytes    BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Cumul par projet et par mois (premier jour du mois, UTC).
CREATE TABLE IF NOT EXISTS vpc_traffic_monthly (
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    month           DATE NOT NULL,
    from_vps_bytes  BIGINT NOT NULL DEFAULT 0,
    to_vps_bytes    BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, month),
    CONSTRAINT vpc_traffic_monthly_month_check CHECK (EXTRACT(DAY FROM month) = 1)
);
CREATE INDEX IF NOT EXISTS idx_vpc_traffic_monthly_month ON vpc_traffic_monthly (month);
