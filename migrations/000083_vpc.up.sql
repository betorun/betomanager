-- VPC, lot 1 (ADR 0019) : réseau privé d'un projet étendu à ses VPS.
-- Modèle seulement : rien n'est posé sur un cluster ni chez un hébergeur.

-- Un VPC par projet, créé au premier rattachement. Sa plage /24 est prise
-- dans 10.248.0.0/13, unique dans le cluster du projet (les VPC sont isolés :
-- deux clusters peuvent réutiliser une plage).
CREATE TABLE IF NOT EXISTS vpc_networks (
    project_id  UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    cluster_id  VARCHAR(100) NOT NULL DEFAULT '',
    cidr        CIDR NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT vpc_networks_cidr_check CHECK (masklen(cidr) = 24 AND cidr <<= '10.248.0.0/13'),
    CONSTRAINT vpc_networks_cluster_cidr_key UNIQUE (cluster_id, cidr)
);

-- Rattachement d'un VPS au VPC de son projet (un seul VPC par VPS).
-- backend : mise en œuvre retenue (interne, jamais rendue par l'API).
-- state : pending (en attente du VPS), attached (relié).
CREATE TABLE IF NOT EXISTS vpc_attachments (
    vps_id           UUID PRIMARY KEY REFERENCES compute_vps(id) ON DELETE CASCADE,
    project_id       UUID NOT NULL REFERENCES vpc_networks(project_id) ON DELETE CASCADE,
    address          INET NOT NULL,
    backend          VARCHAR(32) NOT NULL,
    state            VARCHAR(16) NOT NULL DEFAULT 'pending',
    public_key       TEXT NOT NULL DEFAULT '',
    created_by       VARCHAR(255) NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attached_at      TIMESTAMPTZ,
    last_contact_at  TIMESTAMPTZ,
    CONSTRAINT vpc_attachments_state_check CHECK (state IN ('pending', 'attached')),
    CONSTRAINT vpc_attachments_address_key UNIQUE (project_id, address)
);
CREATE INDEX IF NOT EXISTS idx_vpc_attachments_project ON vpc_attachments (project_id);

-- Jetons d'inscription : à usage unique, 30 min, liés à un VPS. Seule leur
-- empreinte SHA-256 est gardée ; le jeton n'est montré qu'une fois.
CREATE TABLE IF NOT EXISTS vpc_enrollments (
    id          UUID PRIMARY KEY,
    vps_id      UUID NOT NULL REFERENCES vpc_attachments(vps_id) ON DELETE CASCADE,
    token_hash  BYTEA NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_by  VARCHAR(255) NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT vpc_enrollments_hash_check CHECK (octet_length(token_hash) = 32)
);
CREATE INDEX IF NOT EXISTS idx_vpc_enrollments_vps ON vpc_enrollments (vps_id);
