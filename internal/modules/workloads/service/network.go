package service

import (
	"strconv"

	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// InternalURL : adresse d'un conteneur web dans le réseau privé de son projet
// (Service Kubernetes, port 80). Les conteneurs d'un projet partagent un
// espace de noms : le nom court suffit (http://bomba-api), sans dépendre du
// nom de l'espace. Service interne : nom et vrai port (redis-cache:6379),
// sans schéma (le protocole dépend du logiciel). Worker et cron n'écoutent
// aucun port : "".
func InternalURL(w *domain.Workload) string {
	if w.Name == "" {
		return ""
	}
	switch kindOf(w) {
	case domain.KindWeb:
		return "http://" + w.Name
	case domain.KindService:
		if w.TargetPort > 0 {
			return w.Name + ":" + strconv.Itoa(w.TargetPort)
		}
	}
	return ""
}
