package domain

// Raisons Kubernetes des événements de pods transmises par le watcher.
const (
	ReasonCrashLoopBackOff           = "CrashLoopBackOff"
	ReasonImagePullBackOff           = "ImagePullBackOff"
	ReasonCreateContainerConfigError = "CreateContainerConfigError"
	ReasonOOMKilled                  = "OOMKilled"
	ReasonFailed                     = "Failed"
	ReasonEvicted                    = "Evicted"
)

// Résultat de l'enrôlement d'un cluster (EnrollResponse.Status).
const (
	EnrollStatusEnrolled = "enrolled"
	EnrollStatusUpdated  = "updated"
)

// Accusés de réception d'un événement de pod (PodEventAck.Status).
const (
	AckStatusAccepted = "accepted"
	AckStatusIgnored  = "ignored"
	AckStatusMapped   = "mapped"
)

// metricStatusRejected : événement refusé car son namespace ne correspond pas
// au workload visé. Libellé de métrique seulement : le watcher reçoit "ignored".
const MetricStatusRejected = "rejected"

// podEventType : type des messages temps réel d'événement de pod.
const PodEventType = "pod_event"
