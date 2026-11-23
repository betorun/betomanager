package router

import (
	"reflect"

	"github.com/wI2L/fizz"

	alertsDomain "github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
	alertsSvc "github.com/thekrauss/kubemanager/internal/modules/alerts/service"
	computeDomain "github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	computeSvc "github.com/thekrauss/kubemanager/internal/modules/compute/service"
	diskresizeDomain "github.com/thekrauss/kubemanager/internal/modules/diskresize/domain"
	diskusageDomain "github.com/thekrauss/kubemanager/internal/modules/diskusage/domain"
	insightsDomain "github.com/thekrauss/kubemanager/internal/modules/insights/domain"
	legalDomain "github.com/thekrauss/kubemanager/internal/modules/legal/domain"
	legalSvc "github.com/thekrauss/kubemanager/internal/modules/legal/service"
	statusDomain "github.com/thekrauss/kubemanager/internal/modules/status/domain"
	statusSvc "github.com/thekrauss/kubemanager/internal/modules/status/service"
	supportDomain "github.com/thekrauss/kubemanager/internal/modules/support/domain"
	supportSvc "github.com/thekrauss/kubemanager/internal/modules/support/service"
)

// stableSchemaNames : fizz nomme un schéma « paquet + type ». Depuis le
// rangement des modules (docs/architecture/module-layout.md), le paquet vaut
// « domain » ou « service » dans chaque module : ces types gardent le nom
// qu'ils avaient (contrat public de la doc, console, clients de l'API).
func stableSchemaNames(router *fizz.Fizz) {
	for t, name := range map[reflect.Type]string{
		reflect.TypeOf(alertsSvc.CreatedWebhook{}):    "AlertsCreatedWebhook",
		reflect.TypeOf(alertsSvc.Webhook{}):           "AlertsWebhook",
		reflect.TypeOf(alertsSvc.WebhookList{}):       "AlertsWebhookList",
		reflect.TypeOf(alertsDomain.Preferences{}):    "AlertsPreferences",
		reflect.TypeOf(computeDomain.VPS{}):           "ComputeVPS",
		reflect.TypeOf(computeSvc.OfferView{}):        "ComputeOfferView",
		reflect.TypeOf(diskresizeDomain.Resize{}):     "DiskresizeResize",
		reflect.TypeOf(diskusageDomain.Usage{}):       "DiskusageUsage",
		reflect.TypeOf(diskusageDomain.UsageList{}):   "DiskusageUsageList",
		reflect.TypeOf(insightsDomain.InsightList{}):  "InsightsInsightList",
		reflect.TypeOf(insightsDomain.Insight{}):      "InsightsInsight",
		reflect.TypeOf(insightsDomain.Resource{}):     "InsightsResource",
		reflect.TypeOf(legalDomain.Document{}):        "LegalDocument",
		reflect.TypeOf(legalDomain.Content{}):         "LegalContent",
		reflect.TypeOf(legalSvc.Text{}):               "LegalText",
		reflect.TypeOf(legalSvc.OrgDocument{}):        "LegalOrgDocument",
		reflect.TypeOf(legalSvc.KindSummary{}):        "LegalKindSummary",
		reflect.TypeOf(legalSvc.VersionSummary{}):     "LegalVersionSummary",
		reflect.TypeOf(legalSvc.VersionDetail{}):      "LegalVersionDetail",
		reflect.TypeOf(statusDomain.Incident{}):       "StatusIncident",
		reflect.TypeOf(statusDomain.IncidentUpdate{}): "StatusIncidentUpdate",
		reflect.TypeOf(statusDomain.IncidentInput{}):  "StatusIncidentInput",
		reflect.TypeOf(statusDomain.UpdateInput{}):    "StatusUpdateInput",
		reflect.TypeOf(statusSvc.PublicStatus{}):      "StatusPublicStatus",
		reflect.TypeOf(statusSvc.PublicComponent{}):   "StatusPublicComponent",
		reflect.TypeOf(statusSvc.PublicIncident{}):    "StatusPublicIncident",
		reflect.TypeOf(statusSvc.PublicUpdate{}):      "StatusPublicUpdate",
		reflect.TypeOf(statusSvc.AdminStatus{}):       "StatusAdminStatus",
		reflect.TypeOf(supportDomain.Ticket{}):        "SupportTicket",
		reflect.TypeOf(supportDomain.Message{}):       "SupportMessage",
		reflect.TypeOf(supportDomain.Attachment{}):    "SupportAttachment",
		reflect.TypeOf(supportDomain.Event{}):         "SupportTicketEvent",
		reflect.TypeOf(supportDomain.Macro{}):         "SupportMacro",
		reflect.TypeOf(supportDomain.Summary{}):       "SupportSummary",
		reflect.TypeOf(supportSvc.TicketView{}):       "SupportTicketResponse",
		reflect.TypeOf(supportSvc.TicketList{}):       "SupportListTicketsResponse",
		reflect.TypeOf(statusSvc.AdminComponent{}):    "StatusAdminComponent",
		reflect.TypeOf(statusSvc.ResolveInput{}):      "StatusResolveInput",
	} {
		_ = router.Generator().OverrideTypeName(t, name)
	}
}
