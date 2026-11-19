-- Facturation, phase 5 (docs/adr/0012-facturation.md) : plans liés aux
-- limites. Un plan fixe les limites de chaque projet de l'organisation et un
-- abonnement mensuel (mesuré comme une ressource : à l'heure, plafonné au
-- mois). Les plans vivent en base, modifiables par l'équipe ; aucun montant
-- dans le code.
CREATE TABLE IF NOT EXISTS billing_plans (
    id            VARCHAR(30) PRIMARY KEY,
    name          TEXT        NOT NULL,
    position      INTEGER     NOT NULL DEFAULT 0,
    -- public : proposé aux clients ; sinon, attribué par l'équipe seulement.
    public        BOOLEAN     NOT NULL DEFAULT TRUE,
    cpu_limit     VARCHAR(20) NOT NULL,
    memory_limit  VARCHAR(20) NOT NULL,
    storage_limit VARCHAR(20) NOT NULL,
    -- fees : abonnement mensuel par monnaie, en unités ({"XAF": 5000}).
    fees          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_by    TEXT        NOT NULL DEFAULT '',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Plans de départ. Gratuit reprend les limites par défaut des projets : les
-- comptes existants ne changent pas.
INSERT INTO billing_plans (id, name, position, cpu_limit, memory_limit, storage_limit, fees, updated_by) VALUES
    ('free',    'Gratuit', 0, '2000m', '4Gi',  '10Gi',  '{"XAF": 0, "XOF": 0}',         'migration'),
    ('starter', 'Starter', 1, '4000m', '8Gi',  '40Gi',  '{"XAF": 5000, "XOF": 5000}',   'migration'),
    ('pro',     'Pro',     2, '8000m', '16Gi', '160Gi', '{"XAF": 15000, "XOF": 15000}', 'migration')
ON CONFLICT (id) DO NOTHING;

ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS plan_id VARCHAR(30) NOT NULL DEFAULT 'free' REFERENCES billing_plans(id);
ALTER TABLE billing_accounts ADD COLUMN IF NOT EXISTS plan_since TIMESTAMPTZ;
