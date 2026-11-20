-- Langue préférée d'un compte (e-mails envoyés hors requête : alertes du
-- worker, avis d'accès) et alertes coupées par personne.
ALTER TABLE users ADD COLUMN IF NOT EXISTS locale VARCHAR(5) NOT NULL DEFAULT '';

-- Une ligne = cette personne ne reçoit plus cette catégorie d'alertes de
-- projet. Les e-mails de sécurité, d'accès et de facturation ne se coupent pas.
CREATE TABLE alert_mutes (
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    category   VARCHAR(30) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, category)
);
