package router

import (
	"reflect"

	"github.com/wI2L/fizz"

	alertsDomain "github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
	alertsSvc "github.com/thekrauss/kubemanager/internal/modules/alerts/service"
	diskresizeDomain "github.com/thekrauss/kubemanager/internal/modules/diskresize/domain"
	diskusageDomain "github.com/thekrauss/kubemanager/internal/modules/diskusage/domain"
)

// stableSchemaNames : fizz nomme un schéma « paquet + type ». Depuis le
// rangement des modules (docs/architecture/module-layout.md), le paquet vaut
// « domain » ou « service » dans chaque module : ces types gardent le nom
// qu'ils avaient (contrat public de la doc, console, clients de l'API).
func stableSchemaNames(router *fizz.Fizz) {
	for t, name := range map[reflect.Type]string{
		reflect.TypeOf(alertsSvc.CreatedWebhook{}):  "AlertsCreatedWebhook",
		reflect.TypeOf(alertsSvc.Webhook{}):         "AlertsWebhook",
		reflect.TypeOf(alertsSvc.WebhookList{}):     "AlertsWebhookList",
		reflect.TypeOf(alertsDomain.Preferences{}):  "AlertsPreferences",
		reflect.TypeOf(diskresizeDomain.Resize{}):   "DiskresizeResize",
		reflect.TypeOf(diskusageDomain.Usage{}):     "DiskusageUsage",
		reflect.TypeOf(diskusageDomain.UsageList{}): "DiskusageUsageList",
	} {
		_ = router.Generator().OverrideTypeName(t, name)
	}
}
