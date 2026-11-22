-- E-mails (docs/modules/mail.md) : adresses à qui plus rien n'est envoyé.
-- Alimentée par les refus définitifs du serveur SMTP (550, 551, 553) et par
-- le webhook d'événements de SendGrid (rebonds, plaintes). L'équipe relâche
-- une adresse depuis l'administration.
CREATE TABLE IF NOT EXISTS mail_suppressions (
    email      TEXT PRIMARY KEY,
    reason     TEXT        NOT NULL,
    source     TEXT        NOT NULL,
    detail     TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_mail_suppressions_created ON mail_suppressions(created_at DESC);
