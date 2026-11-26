-- Catalogue retiré : les régions restent (codes et adresses de mesure), sans
-- zone, pays ni ville.
ALTER TABLE region_settings DROP CONSTRAINT IF EXISTS region_settings_place_check;
ALTER TABLE region_settings DROP CONSTRAINT IF EXISTS region_settings_country_check;
ALTER TABLE region_settings DROP CONSTRAINT IF EXISTS region_settings_zone_check;
ALTER TABLE region_settings DROP CONSTRAINT IF EXISTS region_settings_code_check;
ALTER TABLE region_settings DROP COLUMN IF EXISTS created_at;
ALTER TABLE region_settings DROP COLUMN IF EXISTS city;
ALTER TABLE region_settings DROP COLUMN IF EXISTS country;
ALTER TABLE region_settings DROP COLUMN IF EXISTS zone;
