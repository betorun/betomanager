package api

import (
	"context"
	"net/http"

	diskusagedomain "github.com/thekrauss/kubemanager/internal/modules/diskusage/domain"
)

// DiskUsage : usage réel des disques d'un projet (mesuré toutes les heures).
type DiskUsage = diskusagedomain.UsageList

func (c *Client) DiskUsage(ctx context.Context, projectID string) (*DiskUsage, error) {
	var out DiskUsage
	return &out, c.Do(ctx, http.MethodGet, "/projects/"+projectID+"/disk-usage", nil, &out)
}
