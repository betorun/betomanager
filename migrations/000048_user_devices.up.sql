-- Appareils déjà vus à la connexion d'un compte : une connexion depuis un
-- appareil inconnu envoie un e-mail à son titulaire. L'appareil est décrit par
-- son type de client, navigateur et système (pas d'adresse IP : elle change
-- trop souvent sur mobile).
CREATE TABLE user_devices (
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    fingerprint VARCHAR(64) NOT NULL,
    label       VARCHAR(120) NOT NULL,
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, fingerprint)
);
