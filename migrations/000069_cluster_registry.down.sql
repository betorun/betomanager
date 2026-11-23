DROP TABLE IF EXISTS cluster_health;
DROP INDEX IF EXISTS uq_clusters_default;
ALTER TABLE clusters DROP CONSTRAINT IF EXISTS clusters_state_check;
ALTER TABLE clusters DROP CONSTRAINT IF EXISTS clusters_distribution_check;
ALTER TABLE clusters DROP CONSTRAINT IF EXISTS clusters_provider_check;
ALTER TABLE clusters
    DROP COLUMN IF EXISTS declared_by,
    DROP COLUMN IF EXISTS kubeconfig_set_by,
    DROP COLUMN IF EXISTS kubeconfig_set_at,
    DROP COLUMN IF EXISTS api_server,
    DROP COLUMN IF EXISTS kubeconfig_ciphertext,
    DROP COLUMN IF EXISTS notes,
    DROP COLUMN IF EXISTS is_default,
    DROP COLUMN IF EXISTS state,
    DROP COLUMN IF EXISTS region,
    DROP COLUMN IF EXISTS distribution,
    DROP COLUMN IF EXISTS provider,
    DROP COLUMN IF EXISTS name;
UPDATE clusters SET enrolled_at = created_at WHERE enrolled_at IS NULL;
ALTER TABLE clusters ALTER COLUMN enrolled_at SET DEFAULT NOW();
ALTER TABLE clusters ALTER COLUMN enrolled_at SET NOT NULL;
