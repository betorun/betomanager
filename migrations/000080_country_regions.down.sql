DROP TABLE IF EXISTS region_settings;
DROP TABLE IF EXISTS country_regions;
ALTER TABLE organizations DROP COLUMN IF EXISTS country;
