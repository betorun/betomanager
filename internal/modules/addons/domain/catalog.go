package domain

import (
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Catalogue fermé, maintenu dans le code : ajouter un logiciel = une revue
// de code (ADR 0013). Chaque version fige une image précise ; on la monte
// nous-mêmes quand un correctif sort.

// Version : une version maintenue d'un modèle.
type Version struct {
	ID    string `json:"id"`
	Image string `json:"-"`
	Major int    `json:"major"`
}

// Mode : usage proposé (redis : persistant ou cache seulement).
type Mode struct {
	ID string `json:"id"`
	// Persistent : un disque garde les données entre deux redémarrages.
	Persistent bool `json:"persistent"`
}

// Size : ressources d'une instance (comptées dans le quota du projet).
type Size struct {
	ID         string `json:"id"`
	CPURequest string `json:"cpu_request"`
	CPULimit   string `json:"cpu_limit"`
	Memory     string `json:"memory"`
}

// Sizes : tailles communes à tous les modèles. La mémoire réservée vaut la
// limite : un service à état ne doit pas être évincé faute de mémoire.
var Sizes = []Size{
	{ID: "mini", CPURequest: "50m", CPULimit: "250m", Memory: "256Mi"},
	{ID: "small", CPURequest: "100m", CPULimit: "500m", Memory: "512Mi"},
	{ID: "medium", CPURequest: "200m", CPULimit: "1000m", Memory: "1Gi"},
}

// DefaultSize : taille proposée par défaut.
const DefaultSize = "small"

func SizeByID(id string) (Size, bool) {
	for _, s := range Sizes {
		if s.ID == id {
			return s, true
		}
	}
	return Size{}, false
}

// Process : ce que le conteneur porteur exécute.
type Process struct {
	Command []string
	// Health : sonde de vivacité (commande) ; le port est vérifié en plus.
	Health []string
}

// Template : un logiciel du catalogue.
type Template struct {
	ID string `json:"id"`
	// Title : le nom que les clients cherchent (« Redis (via Valkey) ») ;
	// Logo : clé du logo côté console, celui du logiciel RÉELLEMENT installé.
	Title string `json:"title"`
	Logo  string `json:"logo"`
	// Category : rangement du catalogue (databases, cache, search).
	Category string `json:"category"`
	Port     int    `json:"port"`
	Scheme   string `json:"scheme"`
	User     string `json:"-"`
	// Variable : variable posée par défaut sur les conteneurs branchés.
	Variable    string    `json:"variable"`
	Versions    []Version `json:"versions"` // la recommandée d'abord
	Modes       []Mode    `json:"modes"`    // le défaut d'abord
	Disks       []string  `json:"disks"`    // le défaut d'abord
	DiskPath    string    `json:"-"`
	RunAsUser   int64     `json:"-"`
	process     func(mode Mode, memoryBytes int64) Process
	Recommended string `json:"recommended_version"`
}

// Redis : Valkey (licence BSD), compatible avec les clients Redis. La carte
// s'appelle « Redis (via Valkey) » et montre le logo Valkey, jamais celui de
// Redis (marque de Redis Ltd).
var Redis = Template{
	ID:          "redis",
	Title:       "Redis (via Valkey)",
	Logo:        "valkey",
	Category:    "cache",
	Port:        6379,
	Scheme:      "redis",
	User:        "default",
	Variable:    "REDIS_URL",
	Recommended: "9.1",
	Versions: []Version{
		{ID: "9.1", Image: "valkey/valkey:9.1.4", Major: 9},
		{ID: "9.0", Image: "valkey/valkey:9.0.8", Major: 9},
		{ID: "8.1", Image: "valkey/valkey:8.1.12", Major: 8},
	},
	Modes:     []Mode{{ID: "persistent", Persistent: true}, {ID: "cache"}},
	Disks:     []string{"1Gi", "2Gi", "5Gi"},
	DiskPath:  "/data",
	RunAsUser: 999, // utilisateur « valkey » de l'image, propriétaire de /data
	process: func(mode Mode, memoryBytes int64) Process {
		// maxmemory ≈ 80 % de la limite : Valkey refuse proprement une
		// écriture (ou évince en cache) au lieu d'être tué faute de mémoire.
		cmd := []string{"valkey-server",
			"--requirepass", "$(" + PasswordVariable + ")",
			"--maxmemory", strconv.FormatInt(memoryBytes*8/10, 10),
		}
		if mode.Persistent {
			// Files de tâches : ne jamais perdre un travail ; journal sur disque.
			cmd = append(cmd, "--maxmemory-policy", "noeviction", "--appendonly", "yes", "--dir", "/data")
		} else {
			cmd = append(cmd, "--maxmemory-policy", "allkeys-lru", "--appendonly", "no", "--save", "")
		}
		// Le mot de passe passe par l'environnement, jamais par les arguments.
		health := []string{"sh", "-c", `VALKEYCLI_AUTH="$` + PasswordVariable + `" valkey-cli ping | grep -q PONG`}
		return Process{Command: cmd, Health: health}
	},
}

// Catalog : modèles ouverts, dans l'ordre d'affichage.
var Catalog = []Template{Redis}

// Upcoming : service annoncé « Bientôt » ; on peut demander à être prévenu.
// Quand il entre dans Catalog, il sort d'ici et les demandes reçoivent un
// e-mail (une seule fois).
type Upcoming struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Logo     string `json:"logo"`
	Category string `json:"category"`
	// Compatible : protocole repris (« MySQL »), vide sinon.
	Compatible string `json:"compatible,omitempty"`
}

// Announced : services annoncés, dans l'ordre d'affichage.
var Announced = []Upcoming{
	{ID: "mariadb", Title: "MariaDB", Logo: "mariadb", Category: "databases", Compatible: "MySQL"},
	{ID: "meilisearch", Title: "Meilisearch", Logo: "meilisearch", Category: "search"},
}

// UpcomingByID : service annoncé (pas encore ouvert).
func UpcomingByID(id string) (Upcoming, bool) {
	for _, u := range Announced {
		if u.ID == id {
			return u, true
		}
	}
	return Upcoming{}, false
}

// TemplateByID : modèle du catalogue.
func TemplateByID(id string) (Template, bool) {
	for _, t := range Catalog {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// Version : version maintenue (vide : la recommandée).
func (t Template) Version(id string) (Version, int, bool) {
	if id == "" {
		return t.Versions[0], 0, true
	}
	for i, v := range t.Versions {
		if v.ID == id {
			return v, i, true
		}
	}
	return Version{}, -1, false
}

// Latest : la version la plus récente maintenue (la première).
func (t Template) Latest() Version { return t.Versions[0] }

// Mode : usage proposé (vide : le premier).
func (t Template) Mode(id string) (Mode, bool) {
	if id == "" && len(t.Modes) > 0 {
		return t.Modes[0], true
	}
	for _, m := range t.Modes {
		if m.ID == id {
			return m, true
		}
	}
	return Mode{}, false
}

// ValidDisk : disque proposé.
func (t Template) ValidDisk(d string) bool {
	for _, x := range t.Disks {
		if x == d {
			return true
		}
	}
	return false
}

// Process : commande et sonde de l'instance, pour ce mode et cette taille.
func (t Template) Process(mode Mode, size Size) Process {
	mem := resource.MustParse(size.Memory)
	return t.process(mode, mem.Value())
}

// URI : adresse de connexion remise aux conteneurs branchés (mot de passe
// compris : hexadécimal, rien à échapper).
func (t Template) URI(host, password string) string {
	return t.Scheme + "://" + t.User + ":" + password + "@" + host + ":" + strconv.Itoa(t.Port)
}

// Newer : version cible plus récente que la courante (jamais de retour en
// arrière : le format des données ne le permet pas).
func (t Template) Newer(current, target string) (Version, bool) {
	_, ci, ok := t.Version(current)
	tv, ti, ok2 := t.Version(target)
	if !ok2 {
		return Version{}, false
	}
	if !ok { // version courante retirée du catalogue : toute version maintenue va
		return tv, true
	}
	return tv, ti < ci
}
