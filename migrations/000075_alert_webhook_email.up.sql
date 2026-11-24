-- Webhooks d'alertes « E-mail » : une adresse en plus des propriétaires et
-- éditeurs, qui ne reçoit rien tant qu'elle n'a pas confirmé (lien envoyé à
-- l'adresse). Les webhooks existants (Slack, JSON, Telegram) sont confirmés.
ALTER TABLE alert_webhooks ADD COLUMN confirmed_at TIMESTAMPTZ;
UPDATE alert_webhooks SET confirmed_at = created_at;
