-- IAM phase C : connexion d'entreprise par organisation (OpenID Connect,
-- iam.md §9.2). Le secret du client est chiffré (même clé que le STS).
CREATE TABLE IF NOT EXISTS org_sso (
    organization_id   UUID PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    issuer            VARCHAR(300) NOT NULL,
    client_id         VARCHAR(255) NOT NULL,
    client_secret_enc BYTEA        NOT NULL,
    enabled           BOOLEAN      NOT NULL DEFAULT FALSE,
    enforced          BOOLEAN      NOT NULL DEFAULT FALSE,
    jit_role          VARCHAR(100) NOT NULL DEFAULT '',
    tested_at         TIMESTAMPTZ,
    updated_by        TEXT         NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Domaines revendiqués, prouvés par un enregistrement TXT. Un domaine
-- vérifié n'appartient qu'à une organisation.
CREATE TABLE IF NOT EXISTS org_sso_domains (
    organization_id UUID         NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    domain          VARCHAR(253) NOT NULL,
    token           VARCHAR(64)  NOT NULL,
    verified_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (organization_id, domain)
);
CREATE UNIQUE INDEX IF NOT EXISTS org_sso_domains_verified ON org_sso_domains (domain) WHERE verified_at IS NOT NULL;

-- Identités liées : « sso:<organisation> » dépasse 20 caractères.
ALTER TABLE user_identities ALTER COLUMN provider TYPE VARCHAR(60);
