package service

import (
	"testing"

	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

func TestInternalURL(t *testing.T) {
	cases := map[string]struct {
		w    domain.Workload
		want string
	}{
		"web":              {domain.Workload{Name: "bomba-api", Namespace: "km-abc", Kind: domain.KindWeb}, "http://bomba-api.km-abc.svc"},
		"ancien sans kind": {domain.Workload{Name: "site", Namespace: "km-abc"}, "http://site.km-abc.svc"},
		"worker":           {domain.Workload{Name: "w", Namespace: "km-abc", Kind: domain.KindWorker}, ""},
		"cron":             {domain.Workload{Name: "c", Namespace: "km-abc", Kind: domain.KindCron}, ""},
	}
	for name, c := range cases {
		if got := InternalURL(&c.w); got != c.want {
			t.Errorf("%s : %q", name, got)
		}
	}
}
