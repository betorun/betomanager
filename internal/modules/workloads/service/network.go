package service

import "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"

// InternalURL : adresse d'un conteneur web dans le réseau privé de son projet
// (Service Kubernetes, port 80). Les conteneurs d'un projet partagent un
// espace de noms : le nom court suffit (http://bomba-api), sans dépendre du
// nom de l'espace. Worker et cron n'écoutent aucun port : "".
func InternalURL(w *domain.Workload) string {
	if kindOf(w) != domain.KindWeb || w.Name == "" {
		return ""
	}
	return "http://" + w.Name
}
