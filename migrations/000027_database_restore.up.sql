-- Restauration : une base peut naître des sauvegardes d'une autre.
ALTER TABLE databases ADD COLUMN IF NOT EXISTS restored_from UUID;
ALTER TABLE databases ADD COLUMN IF NOT EXISTS restore_target TIMESTAMPTZ;
