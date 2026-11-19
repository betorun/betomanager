package service

import "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"

// InternalURL : adresse d'un conteneur web dans le réseau privé de son projet
// (Service Kubernetes, port 80). Worker et cron n'écoutent aucun port : "".
func InternalURL(w *domain.Workload) string {
	if kindOf(w) != domain.KindWeb || w.Name == "" || w.Namespace == "" {
		return ""
	}
	return "http://" + w.Name + "." + w.Namespace + ".svc"
}
