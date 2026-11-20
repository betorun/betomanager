DELETE FROM user_identities WHERE provider LIKE 'sso:%';
ALTER TABLE user_identities ALTER COLUMN provider TYPE VARCHAR(20);
DROP TABLE IF EXISTS org_sso_domains;
DROP TABLE IF EXISTS org_sso;
