package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// Buckets de stockage (module storages3, ADR 0015) : 80 % du quota, puis
// quota dépassé (le bucket passe en lecture seule). Même catégorie que les
// disques : on les coupe ensemble.

// KindBucketQuota : préfixe des types d'alerte (disk.bucket.80, disk.bucket.100).
const KindBucketQuota = KindDisk + "bucket."

var bucketQuotaCopies = map[int]map[i18n.Locale]alertCopy{
	80: pair(
		alertCopy{"Le bucket « {{.Name}} » est presque plein", "Bucket presque plein",
			"Le bucket « {{.Name}} » (projet « {{.Project}} ») occupe plus de 80 % de son quota : {{.Usage}}. Supprimez des fichiers ou augmentez le quota : une fois le quota atteint, le bucket passera en lecture seule.",
			"Voir les services"},
		alertCopy{"The bucket \"{{.Name}}\" is almost full", "Bucket almost full",
			"The bucket \"{{.Name}}\" (project \"{{.Project}}\") uses more than 80% of its quota: {{.Usage}}. Delete files or raise the quota: once the quota is reached, the bucket will become read-only.",
			"View services"}),
	100: pair(
		alertCopy{"Le bucket « {{.Name}} » est en lecture seule : quota atteint", "Bucket en lecture seule",
			"Le bucket « {{.Name}} » (projet « {{.Project}} ») a atteint son quota : {{.Usage}}. Vos applications peuvent encore lire et supprimer des fichiers, plus en écrire. Supprimez des fichiers ou augmentez le quota depuis la console : l'écriture revient au relevé suivant (15 minutes au plus).",
			"Voir les services"},
		alertCopy{"The bucket \"{{.Name}}\" is read-only: quota reached", "Bucket read-only",
			"The bucket \"{{.Name}}\" (project \"{{.Project}}\") reached its quota: {{.Usage}}. Your applications can still read and delete files, but no longer write. Delete files or raise the quota from the console: writing comes back at the next measurement (15 minutes at most).",
			"View services"}),
}

// BucketQuota : bucket au-delà de 80 % ou de 100 % de son quota.
func (n *Notifier) BucketQuota(ctx context.Context, project, bucket uuid.UUID, level int, name string, used, quota int64) {
	if n == nil {
		return
	}
	base, ok := bucketQuotaCopies[level]
	if !ok {
		return
	}
	copies := make(map[i18n.Locale]alertCopy, len(base))
	for lang, c := range base {
		c.Body = strings.ReplaceAll(c.Body, "{{.Usage}}", usageText(lang, used, quota))
		copies[lang] = c
	}
	n.run(ctx, func(ctx context.Context) {
		n.deliver(ctx, project, bucket, fmt.Sprintf("%s%d", KindBucketQuota, level), DiskEvery, copies, name, pageServices)
	})
}
