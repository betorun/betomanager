-- Contraintes retirées ; les pays repris restent (pas de retour des données).
ALTER TABLE region_settings DROP CONSTRAINT IF EXISTS region_settings_probe_url_check;
ALTER TABLE country_regions DROP CONSTRAINT IF EXISTS country_regions_country_check;
ALTER TABLE organizations DROP CONSTRAINT IF EXISTS organizations_country_check;
