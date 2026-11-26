-- VPC (ADR 0019, lot 2) : le VPS doit être sur le réseau privé de son projet.
-- Coché à la commande (ou au rattachement), décoché au détachement ; lu à
-- chaque installation (création, réinstallation) pour l'inscrire à nouveau.
ALTER TABLE compute_vps ADD COLUMN private_network BOOLEAN NOT NULL DEFAULT FALSE;

-- VPS déjà rattachés (lot 1) : le choix est retenu.
UPDATE compute_vps SET private_network = TRUE WHERE id IN (SELECT vps_id FROM vpc_attachments);
