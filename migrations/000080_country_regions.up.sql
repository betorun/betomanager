-- Région suggérée selon le pays du client : pays de l'organisation (la
-- facturation est par organisation), table pays → région gérée depuis le
-- back-office, adresse de mesure de latence de chaque région.
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS country VARCHAR(2) NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS country_regions (
    country    VARCHAR(2)  PRIMARY KEY,
    region     VARCHAR(32) NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by TEXT        NOT NULL DEFAULT ''
);

-- Réglages d'une région (code des clusters) : adresse https que la console
-- appelle depuis le navigateur pour mesurer le temps de réponse.
CREATE TABLE IF NOT EXISTS region_settings (
    region     VARCHAR(32) PRIMARY KEY,
    probe_url  TEXT        NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by TEXT        NOT NULL DEFAULT ''
);
