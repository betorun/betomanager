-- VPS lot 4 : actions du client, une opération à la fois (docs/modules/compute.md).
ALTER TABLE compute_vps
    ADD COLUMN IF NOT EXISTS operation    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS resume_state TEXT NOT NULL DEFAULT '';

-- Une seule demande active par VPS (création, action, arrêt pour impayé…) :
-- deux opérations concurrentes sont refusées par la base elle-même.
CREATE UNIQUE INDEX IF NOT EXISTS uq_compute_requests_active_vps ON compute_requests (vps_id)
    WHERE vps_id IS NOT NULL AND status IN ('pending', 'started');
