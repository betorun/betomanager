-- Catalogue des régions (ADR 0018) : region_settings devient la liste des
-- régions tenue dans le back-office, avec la zone, le pays et la ville de
-- chacune (la console en tire le libellé). Un cluster ne prend plus qu'une
-- région du catalogue.
ALTER TABLE region_settings ADD COLUMN IF NOT EXISTS zone       VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE region_settings ADD COLUMN IF NOT EXISTS country    VARCHAR(2)  NOT NULL DEFAULT '';
ALTER TABLE region_settings ADD COLUMN IF NOT EXISTS city       VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE region_settings ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Régions dont la console connaissait le libellé : zone, pays et ville
-- posés s'ils manquent.
INSERT INTO region_settings (region, zone, country, city, updated_by) VALUES
    ('eu-west-gra',    'eu-west',    'FR', 'Gravelines',  'migration'),
    ('eu-west-rbx',    'eu-west',    'FR', 'Roubaix',     'migration'),
    ('eu-west-sbg',    'eu-west',    'FR', 'Strasbourg',  'migration'),
    ('af-west-dkr',    'af-west',    'SN', 'Dakar',       'migration'),
    ('af-central-bzv', 'af-central', 'CG', 'Brazzaville', 'migration')
ON CONFLICT (region) DO UPDATE SET
    zone = EXCLUDED.zone, country = EXCLUDED.country, city = EXCLUDED.city
WHERE region_settings.zone = '';

-- Régions déjà portées par un cluster ou associées à un pays : reprises au
-- catalogue, à compléter (zone, pays et ville vides).
INSERT INTO region_settings (region, updated_by)
SELECT DISTINCT region, 'migration' FROM clusters WHERE region <> ''
ON CONFLICT (region) DO NOTHING;
INSERT INTO region_settings (region, updated_by)
SELECT DISTINCT region, 'migration' FROM country_regions
ON CONFLICT (region) DO NOTHING;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'region_settings_code_check') THEN
        ALTER TABLE region_settings ADD CONSTRAINT region_settings_code_check
            CHECK (region ~ '^[a-z0-9-]{1,32}$');
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'region_settings_zone_check') THEN
        ALTER TABLE region_settings ADD CONSTRAINT region_settings_zone_check
            CHECK (zone IN ('', 'af-west', 'af-central', 'af-east', 'af-north', 'af-south', 'eu-west', 'eu-central', 'eu-south'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'region_settings_country_check') THEN
        ALTER TABLE region_settings ADD CONSTRAINT region_settings_country_check
            CHECK (country ~ '^([A-Z]{2})?$');
    END IF;
    -- Une région du catalogue se complète entière : zone, pays et ville
    -- ensemble, ou aucun (région reprise ci-dessus).
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'region_settings_place_check') THEN
        ALTER TABLE region_settings ADD CONSTRAINT region_settings_place_check
            CHECK ((zone = '' AND country = '' AND city = '') OR (zone <> '' AND country <> '' AND city <> ''));
    END IF;
END $$;
