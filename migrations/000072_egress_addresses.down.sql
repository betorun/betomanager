DROP TABLE IF EXISTS egress_addresses;
ALTER TABLE clusters DROP COLUMN IF EXISTS egress_gateway;
