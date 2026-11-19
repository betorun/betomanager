-- Conteneur géré par un autre module (ex. « addon/redis/<id> » : porteur
-- d'un service additionnel). Vide : conteneur du client. Les actions qui
-- casseraient le service (modifier, supprimer, revenir en arrière, dupliquer,
-- toucher aux variables) sont refusées hors de ce module.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS managed_by VARCHAR(80) NOT NULL DEFAULT '';

-- Porteurs créés avant cette colonne : rattachés à leur service, sinon ils
-- resteraient modifiables et supprimables comme un conteneur ordinaire.
UPDATE workloads w
   SET managed_by = 'addon/' || a.template || '/' || a.id::text
  FROM project_addons a
 WHERE a.workload_id = w.id AND w.managed_by = '';
