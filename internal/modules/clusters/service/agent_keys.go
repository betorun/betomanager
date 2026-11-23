package service

import (
	"context"
	"encoding/json"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// ClusterAgentKeys : fichiers de clé du compte système d'un cluster
// (iam.ClusterAgents), limités au compte rendu de ce seul cluster.
type ClusterAgentKeys struct{ Agents *iam.ClusterAgents }

func (k ClusterAgentKeys) Issue(ctx context.Context, by, clusterID string) (string, []byte, error) {
	created, err := k.Agents.CreateKey(ctx, iam.Principal(by), iam.CreateClusterKeyRequest{ClusterID: clusterID,
		Type: iam.ClusterKeyFile, ExpiresInDays: domain.AgentKeyDays, DisplayName: "agent " + clusterID})
	if err != nil {
		return "", nil, err
	}
	if created.KeyFile == nil {
		return "", nil, domain.ErrAgentKeysUnavailable
	}
	raw, err := json.Marshal(created.KeyFile)
	return created.KeyID, raw, err
}

func (k ClusterAgentKeys) Revoke(ctx context.Context, keyID string) error {
	return k.Agents.Revoke(ctx, keyID)
}
