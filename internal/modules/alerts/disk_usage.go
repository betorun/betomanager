package alerts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// Disques au-delà de leur taille (module diskusage) : 80 %, 100 %, puis arrêt
// du conteneur à 120 %. Le texte ne reprend que des noms et des tailles.

// KindDisk : préfixe des types d'alerte de disque (disk.80, disk.100, disk.120).
const KindDisk = "disk."

// DiskEvery : une alerte par seuil et par disque au plus sur cette période
// (l'arrêt, lui, prévient à chaque fois : diskStopEvery).
const (
	DiskEvery     = 24 * time.Hour
	diskStopEvery = time.Hour
)

// diskCopies : par seuil, puis sorte (workload, database). {{.Usage}} est
// remplacé avant le rendu (« 1,2 Go sur 1 Go »).
var diskCopies = map[int]map[string]map[i18n.Locale]alertCopy{
	80: {
		"workload": pair(
			alertCopy{"Le disque de « {{.Name}} » est presque plein", "Disque presque plein",
				"Le disque du conteneur « {{.Name}} » (projet « {{.Project}} ») est rempli à plus de 80 % : {{.Usage}}. Faites de la place, ou prévoyez un disque plus grand. À 120 %, le conteneur sera arrêté (ses données sont gardées).",
				"Voir le conteneur"},
			alertCopy{"The disk of \"{{.Name}}\" is almost full", "Disk almost full",
				"The disk of container \"{{.Name}}\" (project \"{{.Project}}\") is more than 80% full: {{.Usage}}. Free some space or plan a larger disk. At 120%, the container will be stopped (its data is kept).",
				"View the container"}),
		"database": pair(
			alertCopy{"La base « {{.Name}} » est presque pleine", "Base presque pleine",
				"La base « {{.Name}} » (projet « {{.Project}} ») occupe plus de 80 % de son disque : {{.Usage}}. Faites de la place, ou restaurez-la dans une base plus grande.",
				"Voir les services"},
			alertCopy{"The database \"{{.Name}}\" is almost full", "Database almost full",
				"The database \"{{.Name}}\" (project \"{{.Project}}\") uses more than 80% of its disk: {{.Usage}}. Free some space or restore it into a larger database.",
				"View services"}),
	},
	100: {
		"workload": pair(
			alertCopy{"Le disque de « {{.Name}} » est plein", "Disque plein",
				"Le disque du conteneur « {{.Name}} » (projet « {{.Project}} ») dépasse sa taille : {{.Usage}}. Faites de la place rapidement : à 120 %, le conteneur sera arrêté (ses données sont gardées).",
				"Voir le conteneur"},
			alertCopy{"The disk of \"{{.Name}}\" is full", "Disk full",
				"The disk of container \"{{.Name}}\" (project \"{{.Project}}\") exceeds its size: {{.Usage}}. Free some space quickly: at 120%, the container will be stopped (its data is kept).",
				"View the container"}),
		"database": pair(
			alertCopy{"La base « {{.Name}} » est pleine", "Base pleine",
				"La base « {{.Name}} » (projet « {{.Project}} ») dépasse la taille de son disque : {{.Usage}}. Faites de la place rapidement, ou restaurez-la dans une base plus grande.",
				"Voir les services"},
			alertCopy{"The database \"{{.Name}}\" is full", "Database full",
				"The database \"{{.Name}}\" (project \"{{.Project}}\") exceeds its disk size: {{.Usage}}. Free some space quickly, or restore it into a larger database.",
				"View services"}),
	},
	120: {
		"workload": pair(
			alertCopy{"« {{.Name}} » a été arrêté : disque plein", "Conteneur arrêté : disque plein",
				"Le disque du conteneur « {{.Name}} » (projet « {{.Project}} ») dépasse sa taille de plus de 20 % : {{.Usage}}. Le conteneur a été arrêté ; ses données sont gardées. Relancez-le depuis la console et faites de la place dans les 6 heures, sinon il sera de nouveau arrêté.",
				"Voir le conteneur"},
			alertCopy{"\"{{.Name}}\" was stopped: disk full", "Container stopped: disk full",
				"The disk of container \"{{.Name}}\" (project \"{{.Project}}\") exceeds its size by more than 20%: {{.Usage}}. The container was stopped; its data is kept. Start it again from the console and free some space within 6 hours, or it will be stopped again.",
				"View the container"}),
		"database": pair(
			alertCopy{"La base « {{.Name}} » dépasse largement son disque", "Base bien au-delà de son disque",
				"La base « {{.Name}} » (projet « {{.Project}} ») dépasse la taille de son disque de plus de 20 % : {{.Usage}}. Faites de la place dès maintenant, ou restaurez-la dans une base plus grande.",
				"Voir les services"},
			alertCopy{"The database \"{{.Name}}\" far exceeds its disk", "Database far beyond its disk",
				"The database \"{{.Name}}\" (project \"{{.Project}}\") exceeds its disk size by more than 20%: {{.Usage}}. Free some space now, or restore it into a larger database.",
				"View services"}),
	},
}

// usageText : « 1,2 Go sur 1 Go » / « 1.2 GB of 1 GB ».
func usageText(lang i18n.Locale, used, capacity int64) string {
	gb := func(b int64) string {
		s := fmt.Sprintf("%.1f", float64(b)/(1<<30))
		if lang != i18n.EN {
			s = strings.Replace(s, ".", ",", 1)
		}
		return s
	}
	if lang == i18n.EN {
		return fmt.Sprintf("%s GB of %s GB", gb(used), gb(capacity))
	}
	return fmt.Sprintf("%s Go sur %s Go", gb(used), gb(capacity))
}

// DiskUsage : disque au-delà d'un seuil (80, 100 ou 120). Subject : le
// conteneur ou la base.
func (n *Notifier) DiskUsage(ctx context.Context, project, subject uuid.UUID, level int, kind, name string, used, capacity int64) {
	if n == nil {
		return
	}
	byKind, ok := diskCopies[level]
	if !ok {
		return
	}
	base, ok := byKind[kind]
	if !ok {
		return
	}
	copies := make(map[i18n.Locale]alertCopy, len(base))
	for lang, c := range base {
		c.Body = strings.ReplaceAll(c.Body, "{{.Usage}}", usageText(lang, used, capacity))
		copies[lang] = c
	}
	every, page := DiskEvery, pageServices
	if level == 120 && kind == "workload" {
		every = diskStopEvery
	}
	if kind == "workload" {
		page = pageWorkload + subject.String()
	}
	n.run(ctx, func(ctx context.Context) {
		n.deliver(ctx, project, subject, fmt.Sprintf("%s%d", KindDisk, level), every, copies, name, page)
	})
}
