package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// Sortes de conteneur.
const (
	// KindWeb : service HTTP (port, Service, adresse publique, sondes).
	KindWeb = "web"
	// KindWorker : processus continu sans port (file de messages, bot…).
	KindWorker = "worker"
	// KindCron : tâche planifiée (CronJob), une exécution à chaque échéance.
	KindCron = "cron"
	// KindService : service interne TCP (Redis, gRPC…) : son vrai port,
	// joignable par son nom depuis le projet seulement, une instance.
	KindService = "service"
)

const (
	// MaxCommandParts : éléments au plus dans command ou args.
	MaxCommandParts = 32
	// maxCommandPartLen : longueur au plus d'un élément.
	maxCommandPartLen = 4096
	// DefaultCronMaxDuration : durée maximale d'une exécution (1 h).
	DefaultCronMaxDuration = 3600
	// MaxCronMaxDuration : 24 h.
	MaxCronMaxDuration = 86400
)

var (
	ErrInvalidKind      = errors.New("invalid kind: web, worker, cron or service")
	ErrKindImmutable    = errors.New("the kind of a container cannot change: create a new container")
	ErrScheduleRequired = errors.New("a cron container needs a schedule (e.g. \"0 2 * * *\")")
	ErrInvalidSchedule  = errors.New("invalid schedule: 5 fields (minute hour day month weekday) or @hourly, @daily, @weekly, @monthly")
	ErrInvalidCommand   = errors.New("invalid command or args: at most 32 parts of 4096 characters, without NUL")
	ErrNotWeb           = errors.New("this action is only available for a web container")
	ErrPortRequired     = errors.New("a service container needs its port (target_port, e.g. 6379)")
	ErrSingleInstance   = errors.New("a service container runs a single instance: its data lives on its disk")
	ErrInvalidWaitFor   = errors.New("wait_for: at most 8 addresses name:port of the project")
	ErrManaged          = errors.New("this container is managed by a service: change it from the Services page")
	ErrNotCron          = errors.New("this action is only available for a cron container")
	ErrInvalidTimeZone  = errors.New("invalid time zone: use an IANA name such as Africa/Dakar")
	ErrMaxDuration      = errors.New("max_duration_seconds: at most 86400 (24 hours)")
	ErrStartupTimeout   = errors.New("startup_timeout_seconds: between 10 and 600 seconds")
)

// NormalizeKind : sorte connue ; vide = web (conteneurs antérieurs, appels internes).
func NormalizeKind(k string) (string, error) {
	switch strings.TrimSpace(k) {
	case "", KindWeb:
		return KindWeb, nil
	case KindWorker:
		return KindWorker, nil
	case KindCron:
		return KindCron, nil
	case KindService:
		return KindService, nil
	}
	return "", ErrInvalidKind
}

var (
	cronMacro = map[string]bool{"@yearly": true, "@annually": true, "@monthly": true, "@weekly": true, "@daily": true, "@midnight": true, "@hourly": true}
	cronField = regexp.MustCompile(`^(\*|[0-9]{1,2}(-[0-9]{1,2})?|[A-Za-z]{3}(-[A-Za-z]{3})?)(/[0-9]{1,2})?(,(\*|[0-9]{1,2}(-[0-9]{1,2})?|[A-Za-z]{3}(-[A-Za-z]{3})?)(/[0-9]{1,2})?)*$`)
)

// ValidSchedule : horaire au format de Kubernetes (5 champs ou macro). La
// validation fine (bornes) est faite par l'API Kubernetes ; ici, on refuse ce
// qui n'a manifestement pas la forme d'un horaire, avant tout déploiement.
func ValidSchedule(s string) bool {
	s = strings.TrimSpace(s)
	if cronMacro[s] {
		return true
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return false
	}
	for _, f := range fields {
		if !cronField.MatchString(f) {
			return false
		}
	}
	return true
}

// ValidTimeZone : fuseau IANA (vide = UTC).
func ValidTimeZone(tz string) bool {
	if tz == "" {
		return true
	}
	_, err := time.LoadLocation(tz)
	return err == nil && !strings.HasPrefix(tz, "Local")
}

// ValidCommand : command ou args.
func ValidCommand(parts []string) bool {
	if len(parts) > MaxCommandParts {
		return false
	}
	for _, p := range parts {
		if len(p) > maxCommandPartLen || strings.ContainsRune(p, 0) {
			return false
		}
	}
	return true
}

// NormalizeProcess : règles de la sorte, appliquées à la création. Web : un
// port (8080 par défaut). Worker et cron : ni port, ni TLS, ni sondes ; cron :
// un horaire valide, une seule exécution à la fois, ni autoscaling ni PDB.
// Service : un port obligatoire, jamais d'adresse publique, une instance.
func NormalizeProcess(in *CreateWorkloadRequest) error {
	kind, err := NormalizeKind(in.Kind)
	if err != nil {
		return err
	}
	in.Kind = kind
	if !ValidCommand(in.Command) || !ValidCommand(in.Args) || !ValidCommand(in.HealthCommand) {
		return ErrInvalidCommand
	}
	if !ValidStartupTimeout(in.StartupTimeoutSeconds) {
		return ErrStartupTimeout
	}
	if !ValidWaitFor(in.WaitFor) {
		return ErrInvalidWaitFor
	}
	switch kind {
	case KindWeb:
		if in.TargetPort == 0 {
			in.TargetPort = 8080
		}
		if in.Schedule != "" {
			return ErrNotCron
		}
	case KindWorker, KindCron:
		in.TargetPort, in.TLSEnabled, in.NoPublicURL = 0, false, false
		in.HealthPath, in.ReadinessPath = "", ""
	case KindService:
		if in.TargetPort == 0 {
			return ErrPortRequired
		}
		if in.Replicas > 1 || in.Autoscaling != nil {
			return ErrSingleInstance
		}
		in.TLSEnabled, in.NoPublicURL = false, true
		in.HealthPath, in.ReadinessPath = "", ""
		in.Replicas, in.Autoscaling, in.PDBEnabled = 1, nil, false
	}
	if kind != KindWorker && kind != KindService {
		in.HealthCommand = nil // web : sondes HTTP ; cron : une exécution se juge à sa fin
	}
	if kind == KindCron {
		in.Schedule = strings.TrimSpace(in.Schedule)
		if in.Schedule == "" {
			return ErrScheduleRequired
		}
		if !ValidSchedule(in.Schedule) {
			return ErrInvalidSchedule
		}
		if !ValidTimeZone(in.TimeZone) {
			return ErrInvalidTimeZone
		}
		if in.MaxDurationSeconds <= 0 {
			in.MaxDurationSeconds = DefaultCronMaxDuration
		}
		if in.MaxDurationSeconds > MaxCronMaxDuration {
			return ErrMaxDuration
		}
		in.Replicas, in.Autoscaling, in.PDBEnabled = 1, nil, false
	} else if in.Schedule != "" || in.TimeZone != "" {
		return ErrNotCron
	}
	return nil
}

// MaxWaitFor : adresses attendues au plus avant le démarrage.
const MaxWaitFor = 8

var waitForRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,61}[a-z0-9])?:[0-9]{1,5}$`)

// ValidWaitFor : adresses nom:port (noms de Services du projet).
func ValidWaitFor(list []string) bool {
	if len(list) > MaxWaitFor {
		return false
	}
	for _, t := range list {
		if !waitForRe.MatchString(t) {
			return false
		}
	}
	return true
}

// MaxStartupTimeoutSeconds : délai de démarrage toléré au plus.
const MaxStartupTimeoutSeconds = 600

// ValidStartupTimeout : 0 (défaut) ou entre 10 s et 10 min.
func ValidStartupTimeout(s int) bool {
	return s == 0 || (s >= 10 && s <= MaxStartupTimeoutSeconds)
}
