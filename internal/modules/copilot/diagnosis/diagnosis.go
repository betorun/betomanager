// Package diagnosis : diagnostic DÉTERMINISTE d'un conteneur (lot C).
//
// Le Copilot rassemble la fiche, l'état, les événements et les journaux d'un
// conteneur (outil diagnose_workload) ; jusqu'ici le modèle devait tout
// interpréter seul, avec le risque d'inventer une cause. Ce paquet applique
// des règles fixes à ces mêmes données et rend des constats (Finding) :
// chacun cite sa preuve telle quelle (ligne de journal, d'événement, ou champ
// d'état) et, quand la correction est sûre, l'outil d'écriture EXISTANT qui
// la propose (le modèle la présente en carte ; rien n'est exécuté ici).
//
// Paquet pur : aucune entrée-sortie. Les données arrivent déjà lues, avec les
// droits de la personne ; un voisinage illisible est signalé comme inconnu,
// jamais deviné.
package diagnosis

import (
	"sort"
	"strconv"
	"strings"
)

// Severity : gravité d'un constat.
type Severity string

const (
	Danger Severity = "danger"
	Warn   Severity = "warn"
	Info   Severity = "info"
)

// Codes des constats.
const (
	CodeLocalhostDependency = "localhost_dependency"
	CodeUnknownHost         = "unknown_host"
	CodeOutOfMemory         = "out_of_memory"
	CodePortMismatch        = "port_mismatch"
	CodeListensOnLocalhost  = "listens_on_localhost"
	CodeMissingEnv          = "missing_env"
	CodeBadStartCommand     = "bad_start_command"
	CodeWrongArchitecture   = "wrong_architecture"
	CodeImagePull           = "image_pull"
	CodeHealthCheckFailing  = "health_check_failing"
	CodeCrashloopUnknown    = "crashloop_unknown"
	CodeDatabaseStopped     = "database_stopped"
)

// Outils d'écriture visés par les corrections (tous existent au catalogue ;
// la couche outils revérifie chaque correction avec Catalog.Prepare).
const (
	ToolAttachAddon    = "attach_addon"
	ToolAttachDatabase = "attach_database"
	ToolUpdateWorkload = "update_workload"
	ToolStartDatabase  = "start_database"
)

// Bornes.
const (
	// MaxFindings : constats rendus au plus.
	MaxFindings = 5
	// maxEvidence : lignes de preuve par constat.
	maxEvidence = 3
	// maxLineLen : longueur d'une ligne de preuve (coupée au-delà).
	maxLineLen = 300
)

// Fix : correction proposée : un outil d'écriture du catalogue et ses
// arguments. Le modèle la propose en carte ; la personne valide.
type Fix struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// Finding : un constat, toujours avec sa preuve.
type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	// Evidence : lignes EXACTES (journal, événement) ou champs d'état cités.
	Evidence []string          `json:"evidence"`
	Params   map[string]string `json:"params,omitempty"`
	Fix      *Fix              `json:"fix,omitempty"`
}

// Workload : fiche du conteneur diagnostiqué.
type Workload struct {
	ID, ProjectID, Name string
	// Kind : web, worker, cron ou service.
	Kind       string
	Image      string
	TargetPort int
	// MemoryLimit / CPULimit : limites actuelles (ex. 256Mi, 200m).
	MemoryLimit, CPULimit string
	HealthPath            string
	// Variables : variables du conteneur (nil : illisibles). Valeur vide
	// pour un secret : seul son nom est connu.
	Variables []Variable
}

// Variable : une variable du conteneur.
type Variable struct {
	Name   string
	Secret bool
	// Value : valeur d'une variable ordinaire ; toujours vide pour un secret.
	Value string
}

// State : état en direct.
type State struct {
	Status   string
	Restarts int
	// LastReason / LastExitCode : dernière fin d'exécution (OOMKilled,
	// Error…) ; HasExitCode faux si inconnue.
	LastReason   string
	LastExitCode int
	HasExitCode  bool
	// Pull : échec de récupération de l'image (cause, registre), s'il y en a.
	PullCause, PullRegistry string
}

// Event : événement récent du conteneur.
type Event struct {
	Type, Reason, Message string
	Count                 int
}

// Addon : service du projet (Redis, MariaDB, Meilisearch…).
type Addon struct {
	ID, Name, Template, Status string
	// Host / Port : adresse dans le projet, telle que l'API la rend.
	Host string
	Port int
	// Attached : identifiants des conteneurs branchés ; Variables : leurs variables.
	Attached []Attachment
}

// Attachment : un branchement d'un service ou d'une base sur un conteneur.
type Attachment struct {
	WorkloadID, Variable string
}

// Database : base Postgres du projet.
type Database struct {
	ID, Name, Status string
	Host             string
	Port             int
	Attached         []Attachment
}

// Neighbor : autre conteneur du projet.
type Neighbor struct {
	Name, Kind string
	Port       int
	// ManagedBy : conteneur porteur d'un service (déjà vu dans Addons).
	ManagedBy string
}

// Size : taille de conteneur du catalogue de l'API (plus petite d'abord).
type Size struct {
	ID       string
	CPUMilli int64
	MemMi    int64
}

// Input : tout ce que le diagnostic sait, déjà lu.
type Input struct {
	Workload     Workload
	State        State
	Events       []Event
	Logs         string
	PreviousLogs string
	// Voisinage du projet. *Known faux : la liste n'a pas pu être lue (droit
	// absent, erreur) ; le diagnostic ne conclut alors pas à l'absence.
	Addons         []Addon
	AddonsKnown    bool
	Databases      []Database
	DatabasesKnown bool
	Neighbors      []Neighbor
	NeighborsKnown bool
	Sizes          []Size
}

// Analyze : constats, du plus grave au moins grave, MaxFindings au plus.
// Un constat sans preuve n'est jamais rendu.
func Analyze(in Input) []Finding {
	a := &analysis{in: in, lines: logLines(in)}
	var out []Finding
	for _, rule := range []func() []Finding{
		a.imagePull, a.outOfMemory, a.databaseStopped, a.localhostDependency, a.unknownHost,
		a.missingEnv, a.startCommand, a.listening, a.healthCheck,
	} {
		out = append(out, rule()...)
	}
	if f := a.crashloopUnknown(out); f != nil {
		out = append(out, *f)
	}
	kept := out[:0]
	for _, f := range out {
		if len(f.Evidence) > 0 {
			kept = append(kept, f)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return rank(kept[i].Severity) < rank(kept[j].Severity) })
	if len(kept) > MaxFindings {
		kept = kept[:MaxFindings]
	}
	return kept
}

func rank(s Severity) int {
	switch s {
	case Danger:
		return 0
	case Warn:
		return 1
	}
	return 2
}

type analysis struct {
	in Input
	// lines : journaux précédents puis actuels (ordre chronologique).
	lines []string
}

// logLines : lignes non vides, instance précédente d'abord.
func logLines(in Input) []string {
	var out []string
	for _, block := range []string{in.PreviousLogs, in.Logs} {
		for _, l := range strings.Split(block, "\n") {
			if l = strings.TrimRight(l, "\r "); strings.TrimSpace(l) != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

// clip : ligne de preuve bornée (gardée telle quelle sinon).
func clip(l string) string {
	if len(l) <= maxLineLen {
		return l
	}
	cut := maxLineLen
	for cut > 0 && !utf8Start(l[cut]) {
		cut--
	}
	return l[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// evidence : lignes distinctes, bornées, au plus maxEvidence (les dernières).
func evidence(lines ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lines {
		l = clip(l)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	if len(out) > maxEvidence {
		out = out[len(out)-maxEvidence:]
	}
	return out
}

// eventLine : citation d'un événement (raison et message tels quels).
func eventLine(e Event) string {
	return strings.TrimSpace("event " + e.Reason + ": " + e.Message)
}

// stateLine : citation d'un champ d'état.
func stateLine(field, value string) string { return "state " + field + " = " + value }

// memMi : quantité mémoire en Mio (256Mi, 1Gi, 512M, 1G, octets).
func memMi(q string) (int64, bool) {
	q = strings.TrimSpace(q)
	units := []struct {
		suffix string
		mi     float64
	}{{"Ki", 1.0 / 1024}, {"Mi", 1}, {"Gi", 1024}, {"Ti", 1024 * 1024},
		{"k", 1000.0 / 1048576}, {"M", 1e6 / 1048576}, {"G", 1e9 / 1048576}}
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(q, u.suffix), 64)
			if err != nil || n <= 0 {
				return 0, false
			}
			return int64(n*u.mi + 0.5), true
		}
	}
	n, err := strconv.ParseFloat(q, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return int64(n/1048576 + 0.5), true
}

// levenshtein : distance d'édition (noms courts).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
