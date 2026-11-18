-- Alertes email envoyées (docs/adr/0009-alertes-email.md) : une ligne par sujet
-- et par type d'alerte, pour limiter la fréquence des envois, y compris entre
-- plusieurs instances de l'API et après un redémarrage.
CREATE TABLE alert_deliveries (
    subject_id UUID        NOT NULL,
    kind       TEXT        NOT NULL,
    sent_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (subject_id, kind)
);
