ALTER TABLE clusters
    DROP COLUMN IF EXISTS agent_image,
    DROP COLUMN IF EXISTS agent_key_id,
    DROP COLUMN IF EXISTS agent_installed_at,
    DROP COLUMN IF EXISTS agent_installed_by;
