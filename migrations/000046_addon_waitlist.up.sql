-- « Me prévenir » : un utilisateur veut savoir quand un service annoncé
-- « Bientôt » ouvre (ADR 0013). Un seul e-mail par service et par personne
-- (notified_at), dans la langue de la demande. Le compte supprimé emporte
-- ses demandes.
CREATE TABLE IF NOT EXISTS addon_waitlist (
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    template    VARCHAR(40) NOT NULL,
    locale      VARCHAR(5)  NOT NULL DEFAULT 'fr',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    notified_at TIMESTAMPTZ,
    PRIMARY KEY (user_id, template)
);

-- Envoi à l'ouverture : demandes pas encore prévenues d'un service.
CREATE INDEX IF NOT EXISTS idx_addon_waitlist_pending ON addon_waitlist (template) WHERE notified_at IS NULL;
