-- Import qui a restauré la copie (NULL : pas encore restaurée). La console
-- prévient qu'une nouvelle restauration écraserait les écritures faites depuis.
ALTER TABLE database_snapshots
    ADD COLUMN IF NOT EXISTS restore_import_id UUID REFERENCES database_imports(id) ON DELETE SET NULL;
