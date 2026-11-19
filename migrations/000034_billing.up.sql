-- Facturation, phase 2 (docs/adr/0012-facturation.md) : mesure à blanc.

-- Compte de facturation : un par organisation, monnaie fixée à la création.
-- Absent : monnaie par défaut (XAF).
CREATE TABLE IF NOT EXISTS billing_accounts (
    organization_id UUID PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    currency        VARCHAR(3)  NOT NULL DEFAULT 'XAF',
    country         VARCHAR(2)  NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Usage : une ligne par (compteur, ressource, heure), maximum vu dans l'heure.
-- Montants en millièmes d'unité de la monnaie. Pas de clé étrangère vers les
-- ressources : l'usage d'une ressource supprimée reste dû et consultable.
CREATE TABLE IF NOT EXISTS billing_usage (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    hour            TIMESTAMPTZ NOT NULL,
    organization_id UUID        NOT NULL,
    project_id      UUID        NOT NULL,
    meter           VARCHAR(30) NOT NULL,
    resource_id     UUID        NOT NULL,
    resource_name   TEXT        NOT NULL,
    size            VARCHAR(20) NOT NULL DEFAULT '',
    quantity        BIGINT      NOT NULL,
    unit_price      BIGINT      NOT NULL,
    amount          BIGINT      NOT NULL CHECK (amount >= 0),
    currency        VARCHAR(3)  NOT NULL,
    catalog_version TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_billing_usage UNIQUE (meter, resource_id, hour)
);
CREATE INDEX IF NOT EXISTS idx_billing_usage_org_hour ON billing_usage (organization_id, hour);

-- Grilles de prix : une version par modification, la plus récente fait foi.
-- Les montants ne sont pas dans le code : l'équipe de la plateforme les
-- modifie depuis l'administration. Montants en unités de la monnaie, par mois
-- (une minute pour les builds) ; le format est domain.CatalogUpdate.
CREATE TABLE IF NOT EXISTS billing_catalogs (
    currency   VARCHAR(3)  NOT NULL,
    version    INTEGER     NOT NULL CHECK (version > 0),
    data       JSONB       NOT NULL,
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (currency, version)
);

-- Grille de départ (ADR 0012), à recaler pendant la phase de mesure. XAF et
-- XOF : mêmes montants (même arrimage à l'euro).
INSERT INTO billing_catalogs (currency, version, data, created_by)
SELECT c.currency, 1, '{
  "sizes": [
    {"id": "mini",   "cpu_milli": 250,  "memory_mi": 256,  "monthly": 1500},
    {"id": "small",  "cpu_milli": 500,  "memory_mi": 512,  "monthly": 3000},
    {"id": "medium", "cpu_milli": 1000, "memory_mi": 1024, "monthly": 6000},
    {"id": "large",  "cpu_milli": 2000, "memory_mi": 2048, "monthly": 12000},
    {"id": "xlarge", "cpu_milli": 4000, "memory_mi": 4096, "monthly": 24000}
  ],
  "database_monthly": 2000,
  "disk_gi_monthly": 300,
  "build_minute": 10,
  "site_domain_monthly": 0,
  "free_tier": {"compute": 1500, "database": 2300, "build_minutes": 100}
}'::jsonb, 'migration'
FROM (VALUES ('XAF'), ('XOF')) AS c(currency)
ON CONFLICT DO NOTHING;
