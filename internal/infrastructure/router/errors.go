package router

import (
	"errors"
	"net/http"

	clustersDomain "github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
	clustersRepo "github.com/thekrauss/kubemanager/internal/modules/clusters/repository"
	clustersSvc "github.com/thekrauss/kubemanager/internal/modules/clusters/service"
	"github.com/thekrauss/kubemanager/internal/modules/compute"
	computeDomain "github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	legalDomain "github.com/thekrauss/kubemanager/internal/modules/legal/domain"
	mailSvc "github.com/thekrauss/kubemanager/internal/modules/mail/service"
	statusDomain "github.com/thekrauss/kubemanager/internal/modules/status/domain"
	supportDomain "github.com/thekrauss/kubemanager/internal/modules/support/domain"

	"github.com/thekrauss/kubemanager/internal/auth/sso"
	sitesDomain "github.com/thekrauss/kubemanager/internal/modules/sites/domain"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"

	"github.com/gin-gonic/gin"
	"github.com/loopfz/gadgeto/tonic"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/auth/social"
	"github.com/thekrauss/kubemanager/internal/iam"
	idempotency "github.com/thekrauss/kubemanager/internal/infrastructure/idempotence"
	authSvc "github.com/thekrauss/kubemanager/internal/modules/auth/service"
	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	buildsDomain "github.com/thekrauss/kubemanager/internal/modules/builds/domain"
	domainsalesDomain "github.com/thekrauss/kubemanager/internal/modules/domainsales/domain"
	githubDomain "github.com/thekrauss/kubemanager/internal/modules/github/domain"
	identityKC "github.com/thekrauss/kubemanager/internal/modules/identity/keycloak"
	identitySvc "github.com/thekrauss/kubemanager/internal/modules/identity/service"
	projectsSvc "github.com/thekrauss/kubemanager/internal/modules/projects/service"
	registriesDomain "github.com/thekrauss/kubemanager/internal/modules/registries/domain"
	workloadsDomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// ErrorBody : réponse d'erreur. "error" duplique "message" pour les clients
// existants (kweb lit message || error).
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

type sentinelMapping struct {
	err    error
	status int
	code   string
	// detailed : le message complet (contexte ajouté par %w) est sûr à
	// exposer et aide l'utilisateur ("unknown member: user:x@y").
	detailed bool
}

// sentinelErrors : erreurs sentinelles des modules → statut HTTP. Une erreur
// non typée et absente de cette table est une erreur interne (500, message
// générique) : jamais de détail technique renvoyé au client.
var sentinelErrors = []sentinelMapping{
	{gorm.ErrRecordNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},

	{identityKC.ErrInvalidCredentials, http.StatusUnauthorized, betoerrors.CodeUnauthorized, false},
	{identityKC.ErrUserAlreadyExists, http.StatusConflict, betoerrors.CodeConflict, false},
	{identityKC.ErrUserNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{identitySvc.ErrRegisterInputInvalid, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{identitySvc.ErrBootstrapInputInvalid, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{identitySvc.ErrBootstrapProjectRequired, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{identitySvc.ErrUnknownProvider, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{identitySvc.ErrInvalidState, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{identitySvc.ErrEmailNotVerified, http.StatusForbidden, "EMAIL_NOT_VERIFIED", false},

	// Authentification gérée par betorun (docs/architecture/auth.md) : codes
	// stables, exploités par la console et la CLI.
	{identitySvc.ErrInvalidCredentials, http.StatusUnauthorized, "INVALID_CREDENTIALS", false},
	{identitySvc.ErrTooManyAttempts, http.StatusTooManyRequests, "TOO_MANY_ATTEMPTS", false},
	{identitySvc.ErrMFAChallengeExpired, http.StatusUnauthorized, "MFA_CHALLENGE_EXPIRED", false},
	{identitySvc.ErrAuthUnavailable, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", false},
	{identitySvc.ErrWeakPassword, http.StatusBadRequest, "WEAK_PASSWORD", false},
	{identitySvc.ErrCurrentPassword, http.StatusBadRequest, "INVALID_CURRENT_PASSWORD", false},
	{identitySvc.ErrSocialUnavailable, http.StatusNotFound, "PROVIDER_UNAVAILABLE", false},
	{identitySvc.ErrLastSignInMethod, http.StatusConflict, "LAST_SIGN_IN_METHOD", false},
	{social.ErrProviderRefused, http.StatusUnauthorized, "PROVIDER_REFUSED", false},
	{social.ErrEmailUnverified, http.StatusForbidden, "PROVIDER_EMAIL_UNVERIFIED", false},
	{auth.ErrIdentityNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{identitySvc.ErrCLICodeInvalid, http.StatusBadRequest, "CLI_AUTHORIZATION_INVALID", false},
	{identitySvc.ErrInvalidPKCE, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{identitySvc.ErrDeviceCodeUnknown, http.StatusNotFound, "DEVICE_CODE_UNKNOWN", false},
	{auth.ErrInvalidCode, http.StatusBadRequest, "INVALID_CODE", false},
	{auth.ErrPasskeyInvalid, http.StatusUnauthorized, "PASSKEY_INVALID", false},
	{auth.ErrPasskeyUnavailable, http.StatusServiceUnavailable, "PASSKEYS_UNAVAILABLE", false},
	{auth.ErrPasskeyNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{auth.ErrPasskeyLimit, http.StatusConflict, "PASSKEY_LIMIT", false},
	{auth.ErrPasskeyName, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{identitySvc.ErrCeremonyExpired, http.StatusUnauthorized, "PASSKEY_EXPIRED", false},
	{workloadsDomain.ErrDomainNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{workloadsDomain.ErrDomainExists, http.StatusConflict, "CONFLICT", false},
	{workloadsDomain.ErrDomainClaimed, http.StatusConflict, "DOMAIN_CLAIMED", false},
	{workloadsDomain.ErrDomainLimit, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrInvalidPath, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrDomainsDisabled, http.StatusServiceUnavailable, "DOMAINS_UNAVAILABLE", false},
	{sitesDomain.ErrInvalidHostname, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{sitesDomain.ErrPlatformHostname, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrVariableConflict, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrVariableName, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrVariableTooBig, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrTooManyVariable, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrWorkloadStopped, http.StatusConflict, "WORKLOAD_STOPPED", false},
	{workloadsDomain.ErrDeployInProgress, http.StatusConflict, "DEPLOY_IN_PROGRESS", false},
	{workloadsDomain.ErrWorkloadDeleting, http.StatusConflict, "WORKLOAD_DELETING", false},
	{workloadsDomain.ErrWorkloadNotBuilt, http.StatusConflict, "WORKLOAD_NOT_BUILT", false},
	{workloadsDomain.ErrInvalidKind, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrKindImmutable, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrScheduleRequired, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrInvalidSchedule, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrInvalidCommand, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrNotWeb, http.StatusConflict, "NOT_A_WEB_CONTAINER", false},
	{workloadsDomain.ErrNotCron, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrPortRequired, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrSingleInstance, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrInvalidWaitFor, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrManaged, http.StatusConflict, "MANAGED_BY_SERVICE", false},
	{workloadsDomain.ErrDiskResizeRoute, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrInvalidTimeZone, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrMaxDuration, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsDomain.ErrStartupTimeout, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{githubDomain.ErrDisabled, http.StatusNotFound, "GITHUB_DISABLED", false},
	{githubDomain.ErrStateInvalid, http.StatusBadRequest, "GITHUB_STATE_INVALID", false},
	{githubDomain.ErrCodeRequired, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{githubDomain.ErrInstallationDenied, http.StatusForbidden, "GITHUB_INSTALLATION_DENIED", false},
	{githubDomain.ErrInstallationPartial, http.StatusForbidden, "GITHUB_INSTALLATION_PARTIAL", false},
	{githubDomain.ErrInstallationNotLinked, http.StatusNotFound, "GITHUB_INSTALLATION_NOT_LINKED", false},
	{githubDomain.ErrRepositoryInaccessible, http.StatusNotFound, "GITHUB_REPOSITORY_INACCESSIBLE", false},
	{githubDomain.ErrBranchNotFound, http.StatusBadRequest, "GITHUB_BRANCH_NOT_FOUND", false},
	{githubDomain.ErrLinkNotFound, http.StatusNotFound, "GITHUB_LINK_NOT_FOUND", false},

	{registriesDomain.ErrInvalidHost, http.StatusBadRequest, "REGISTRY_INVALID_HOST", false},
	{registriesDomain.ErrPlatformRegistry, http.StatusBadRequest, "REGISTRY_IS_PLATFORM", false},
	{registriesDomain.ErrInvalidUsername, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{registriesDomain.ErrInvalidPassword, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{registriesDomain.ErrNotFound, http.StatusNotFound, "REGISTRY_NOT_FOUND", false},
	{githubDomain.ErrLinkDisabled, http.StatusConflict, "GITHUB_LINK_DISABLED", false},
	{githubDomain.ErrUnavailable, http.StatusServiceUnavailable, "GITHUB_UNAVAILABLE", false},
	{buildsDomain.ErrInvalidName, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{buildsDomain.ErrInvalidFilter, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{buildsDomain.ErrInvalidGitURL, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{buildsDomain.ErrInvalidGitRef, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{buildsDomain.ErrInvalidPath, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{buildsDomain.ErrTooManyBuilds, http.StatusTooManyRequests, "TOO_MANY_BUILDS", false},
	{buildsDomain.ErrImageQuota, http.StatusConflict, "IMAGE_QUOTA", false},
	{buildsDomain.ErrBuildNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{buildsDomain.ErrBuildFinished, http.StatusConflict, "BUILD_FINISHED", false},
	{buildsDomain.ErrNoImage, http.StatusConflict, "NO_IMAGE", false},
	{buildsDomain.ErrImageInUse, http.StatusConflict, "IMAGE_IN_USE", false},
	{buildsDomain.ErrForeignImage, http.StatusForbidden, "FOREIGN_IMAGE", false},
	{auth.ErrNoFactor, http.StatusConflict, "MFA_NOT_ENROLLED", false},
	{auth.ErrFactorExists, http.StatusConflict, "MFA_ALREADY_ENROLLED", false},
	{auth.ErrRecentMFA, http.StatusUnauthorized, "STEP_UP_REQUIRED", false},
	{auth.ErrEmailLinkInvalid, http.StatusBadRequest, "LINK_INVALID", false},
	{auth.ErrRefreshInvalid, http.StatusUnauthorized, "SESSION_EXPIRED", false},
	{auth.ErrRefreshReused, http.StatusUnauthorized, "SESSION_EXPIRED", false},
	{auth.ErrSessionNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{identitySvc.ErrSubjectInvalid, http.StatusUnauthorized, betoerrors.CodeUnauthorized, false},
	{identitySvc.ErrIdentityServiceUnavailable, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", false},
	{identitySvc.ErrMFANotConfigured, http.StatusConflict, "FAILED_PRECONDITION", false},
	{identitySvc.ErrCLIRedirectURI, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{identitySvc.ErrDeviceLoginDisabled, http.StatusNotImplemented, "UNIMPLEMENTED", false},

	{authSvc.ErrUserAPIKeysRetired, http.StatusGone, "GONE", false},
	{idempotency.ErrPayloadMismatch, http.StatusUnprocessableEntity, "IDEMPOTENCY_MISMATCH", false},
	{idempotency.ErrInProgress, http.StatusConflict, betoerrors.CodeConflict, false},

	// IAM : messages rédigés pour l'utilisateur de l'API.
	{iam.ErrResourceNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{iam.ErrEtagMismatch, http.StatusConflict, "ABORTED", false},
	{iam.ErrEtagRequired, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrLastOwner, http.StatusConflict, "FAILED_PRECONDITION", false},
	{iam.ErrEscalation, http.StatusForbidden, "PERMISSION_DENIED", true},
	{iam.ErrUnknownMember, http.StatusBadRequest, "UNKNOWN_MEMBER", true},
	{iam.ErrInvalidPrincipal, http.StatusBadRequest, betoerrors.CodeInvalidInput, true},
	{iam.ErrInvalidBinding, http.StatusBadRequest, betoerrors.CodeInvalidInput, true},
	{iam.ErrUnsupportedCondition, http.StatusBadRequest, betoerrors.CodeInvalidInput, true},
	{iam.ErrInvalidResourceName, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrPolicyNotSupported, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrServiceAccountNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{iam.ErrAPIKeyNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{iam.ErrInvalidAccountID, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrAccountIDTaken, http.StatusConflict, betoerrors.CodeConflict, false},
	{iam.ErrInvalidKeyRestrictions, http.StatusBadRequest, betoerrors.CodeInvalidInput, true},
	{iam.ErrInvalidKeyExpiry, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrActAsRequired, http.StatusForbidden, "PERMISSION_DENIED", false},
	{iam.ErrInvalidClusterID, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrInvalidClusterKeyDays, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrInvalidClusterKeyType, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrInvalidGraceDays, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrClusterKeyNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{iam.ErrClusterKeyInactive, http.StatusConflict, "FAILED_PRECONDITION", false},
	{iam.ErrInvalidPublicKey, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrInvalidSAKeyExpiry, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrTooManySAKeys, http.StatusConflict, "FAILED_PRECONDITION", false},
	{iam.ErrSAKeyNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{iam.ErrInvalidRoleID, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrRoleNotFound, http.StatusNotFound, betoerrors.CodeNotFound, true},
	{iam.ErrRoleIDTaken, http.StatusConflict, betoerrors.CodeConflict, false},
	{iam.ErrInvalidRolePermissions, http.StatusBadRequest, betoerrors.CodeInvalidInput, true},
	{iam.ErrCustomRoleParent, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrCustomRoleOutOfScope, http.StatusBadRequest, betoerrors.CodeInvalidInput, true},
	{iam.ErrRoleEtagMismatch, http.StatusConflict, "ABORTED", false},
	{iam.ErrCustomRoleTitleRequired, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrCustomRoleEscalation, http.StatusForbidden, "PERMISSION_DENIED", false},
	{iam.ErrCustomRolePermissionCount, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrInvalidGroupID, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrGroupIDTaken, http.StatusConflict, betoerrors.CodeConflict, false},
	{iam.ErrGroupNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{iam.ErrNestedGroup, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrGroupMemberNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{iam.ErrGroupEscalation, http.StatusForbidden, "PERMISSION_DENIED", true},
	{iam.ErrGroupOutOfScope, http.StatusBadRequest, betoerrors.CodeInvalidInput, true},
	{iam.ErrDenyPolicyNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{iam.ErrDenyPolicyParent, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrDenyPolicyName, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrDenyPolicyPerms, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrDenyPolicyPrincipals, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrDenyPolicyLimit, http.StatusConflict, "FAILED_PRECONDITION", false},
	{iam.ErrDenyPolicyEtag, http.StatusConflict, "ABORTED", false},
	{iam.ErrDenyPolicyLockout, http.StatusConflict, "FAILED_PRECONDITION", false},
	{iam.ErrInvalidDenyPermission, http.StatusBadRequest, betoerrors.CodeInvalidInput, true},
	{mailSvc.ErrNotSuppressed, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{mailSvc.ErrNoSuppressions, http.StatusServiceUnavailable, "UNAVAILABLE", false},
	// Conditions de vente et politique d'usage (docs/modules/legal.md).
	// Support (docs/modules/support.md).
	{supportDomain.ErrTicketNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{supportDomain.ErrMacroNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{supportDomain.ErrAttachmentNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{supportDomain.ErrInvalidSubject, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrInvalidDescription, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrInvalidBody, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrInvalidStatus, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrInvalidPriority, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrInvalidCategory, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrInvalidTags, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrInvalidResource, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrProjectNotInOrg, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrInvalidAssignee, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrInvalidMacro, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrMessageNotOnTicket, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrAttachmentMissing, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{supportDomain.ErrTicketClosed, http.StatusConflict, "TICKET_CLOSED", false},
	{supportDomain.ErrAttachmentTooLarge, http.StatusRequestEntityTooLarge, "ATTACHMENT_TOO_LARGE", false},
	{supportDomain.ErrAttachmentType, http.StatusBadRequest, "ATTACHMENT_TYPE_NOT_ALLOWED", false},
	{supportDomain.ErrAttachmentLimit, http.StatusConflict, "ATTACHMENT_LIMIT", false},
	{supportDomain.ErrAttachmentsUnavailable, http.StatusServiceUnavailable, "ATTACHMENTS_UNAVAILABLE", false},
	{legalDomain.ErrUnknownKind, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{legalDomain.ErrDocumentNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{legalDomain.ErrNotDraft, http.StatusConflict, "LEGAL_VERSION_PUBLISHED", false},
	{legalDomain.ErrDraftExists, http.StatusConflict, "LEGAL_DRAFT_EXISTS", false},
	{legalDomain.ErrInvalidDocument, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{legalDomain.ErrDocumentTooLong, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{legalDomain.ErrNothingToCopy, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{legalDomain.ErrInvalidLegalQuery, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{legalDomain.ErrNotCurrent, http.StatusConflict, "LEGAL_VERSION_NOT_CURRENT", false},
	{legalDomain.ErrTermsNotAccepted, http.StatusConflict, "TERMS_NOT_ACCEPTED", false},
	// Page de statut (docs/modules/status.md).
	{statusDomain.ErrIncidentNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{statusDomain.ErrIncidentResolved, http.StatusConflict, "INCIDENT_RESOLVED", false},
	{statusDomain.ErrInvalidIncident, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{statusDomain.ErrIncidentTooLong, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{statusDomain.ErrInvalidImpact, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{statusDomain.ErrInvalidStatus, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{statusDomain.ErrInvalidComponents, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{statusDomain.ErrInvalidWindow, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{statusDomain.ErrInvalidStatusUpdate, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrClusterNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{clustersDomain.ErrClusterExists, http.StatusConflict, "ALREADY_EXISTS", false},
	{clustersDomain.ErrInvalidClusterID, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrInvalidProvider, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrInvalidDistribution, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrInvalidState, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrInvalidRegion, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrNameTooLong, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrNotesTooLong, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrDefaultCluster, http.StatusConflict, "DEFAULT_CLUSTER", false},
	{clustersDomain.ErrDisabledDefault, http.StatusConflict, "CLUSTER_DISABLED", false},
	{clustersDomain.ErrClusterUnreachable, http.StatusUnprocessableEntity, "CLUSTER_UNREACHABLE", false},
	{clustersDomain.ErrAccessDenied, http.StatusUnprocessableEntity, "CLUSTER_ACCESS_DENIED", false},
	{clustersDomain.ErrNoClusterInRegion, http.StatusConflict, "NO_CAPACITY_IN_REGION", false},
	{clustersDomain.ErrInvalidRegionCode, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrClusterHasProjects, http.StatusConflict, "CLUSTER_HAS_PROJECTS", false},
	{clustersDomain.ErrInvalidSetting, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrAgentImageMissing, http.StatusBadRequest, "AGENT_IMAGE_MISSING", false},
	{clustersDomain.ErrInvalidAgentImage, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{clustersDomain.ErrAgentAPIURLMissing, http.StatusServiceUnavailable, "UNAVAILABLE", false},
	{clustersDomain.ErrAgentKeysUnavailable, http.StatusServiceUnavailable, "UNAVAILABLE", false},
	{clustersDomain.ErrAdminRequired, http.StatusUnprocessableEntity, "CLUSTER_ADMIN_REQUIRED", false},
	{clustersDomain.ErrPlatformTokenPending, http.StatusGatewayTimeout, "CLUSTER_TOKEN_PENDING", false},
	{clustersDomain.ErrClusterDisabled, http.StatusConflict, "CLUSTER_DISABLED", false},
	{clustersRepo.ErrKubeconfigInvalid, http.StatusBadRequest, "KUBECONFIG_INVALID", false},
	{clustersRepo.ErrKubeconfigExec, http.StatusBadRequest, "KUBECONFIG_EXEC_FORBIDDEN", false},
	{clustersRepo.ErrKubeconfigFiles, http.StatusBadRequest, "KUBECONFIG_FILES_FORBIDDEN", false},
	{clustersRepo.ErrKubeconfigHTTPS, http.StatusBadRequest, "KUBECONFIG_HTTPS_REQUIRED", false},
	{clustersRepo.ErrKubeconfigNoContext, http.StatusBadRequest, "KUBECONFIG_INVALID", false},
	{clustersRepo.ErrKubeconfigNoCredentials, http.StatusBadRequest, "KUBECONFIG_INVALID", false},
	{clustersSvc.ErrNoSealer, http.StatusServiceUnavailable, "UNAVAILABLE", false},
	{computeDomain.ErrOfferNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{computeDomain.ErrInvalidOffer, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{computeDomain.ErrUnavailable, http.StatusServiceUnavailable, "UNAVAILABLE", false},
	{compute.ErrRequestNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{computeDomain.ErrNotSold, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{computeDomain.ErrPriceChanged, http.StatusConflict, "VPS_PRICE_CHANGED", false},
	{computeDomain.ErrInvalidName, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{computeDomain.ErrNameTaken, http.StatusConflict, "ALREADY_EXISTS", false},
	{computeDomain.ErrInvalidSSHKey, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{computeDomain.ErrVPSNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{computeDomain.ErrProjectNotInOrganization, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{computeDomain.ErrBusy, http.StatusConflict, "VPS_BUSY", false},
	{computeDomain.ErrNotReady, http.StatusConflict, "VPS_NOT_READY", false},
	{computeDomain.ErrSuspended, http.StatusConflict, "VPS_SUSPENDED", false},
	{computeDomain.ErrBackupsUnavailable, http.StatusConflict, "BACKUPS_UNAVAILABLE", false},
	{computeDomain.ErrSnapshotNotSubscribed, http.StatusConflict, "SNAPSHOT_NOT_SUBSCRIBED", false},
	{computeDomain.ErrSnapshotExists, http.StatusConflict, "SNAPSHOT_EXISTS", false},
	{computeDomain.ErrNoSnapshot, http.StatusNotFound, "NO_SNAPSHOT", false},
	{computeDomain.ErrRestorePointNotFound, http.StatusNotFound, "RESTORE_POINT_NOT_FOUND", false},
	{computeDomain.ErrSnapshotNotSold, http.StatusConflict, "SNAPSHOT_NOT_SOLD", false},
	{computeDomain.ErrInRescue, http.StatusConflict, "VPS_IN_RESCUE", false},
	{computeDomain.ErrNotInRescue, http.StatusConflict, "VPS_NOT_IN_RESCUE", false},
	{computeDomain.ErrRescueSecretUnavailable, http.StatusGone, "RESCUE_PASSWORD_UNAVAILABLE", false},
	{computeDomain.ErrInvalidReverse, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{computeDomain.ErrReverseNotPointing, http.StatusConflict, "REVERSE_NOT_POINTING", false},
	{computeDomain.ErrNoAddress, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{computeDomain.ErrNotSpamBlocked, http.StatusConflict, "NOT_SPAM_BLOCKED", false},
	{computeDomain.ErrOutOfStock, http.StatusConflict, "OUT_OF_STOCK", false},
	{computeDomain.ErrQuotaReached, http.StatusConflict, "VPS_QUOTA_REACHED", false},
	{computeDomain.ErrNewAccountLimit, http.StatusConflict, "NEW_ACCOUNT_LIMIT", false},
	{computeDomain.ErrNotAbuseSuspended, http.StatusConflict, "NOT_ABUSE_SUSPENDED", false},
	{computeDomain.ErrNotOrphan, http.StatusConflict, "NOT_ORPHAN", false},
	{iam.ErrMFANotConfigured, http.StatusConflict, "FAILED_PRECONDITION", false},
	{iam.ErrInvalidDomain, http.StatusBadRequest, betoerrors.CodeInvalidInput, true},
	{sso.ErrNoSSO, http.StatusNotFound, "SSO_NOT_CONFIGURED", false},
	{sso.ErrNotConfigured, http.StatusNotFound, "SSO_NOT_CONFIGURED", false},
	{sso.ErrSSORequired, http.StatusForbidden, "SSO_REQUIRED", false},
	{sso.ErrEmailDomain, http.StatusForbidden, "SSO_EMAIL_DOMAIN", false},
	{sso.ErrProviderRefused, http.StatusUnauthorized, "SSO_REFUSED", false},
	{sso.ErrDiscovery, http.StatusBadRequest, "SSO_DISCOVERY_FAILED", false},
	{sso.ErrInvalidIssuer, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{sso.ErrClientID, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{sso.ErrClientSecret, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{sso.ErrJITRole, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{sso.ErrEnforceUntested, http.StatusConflict, "FAILED_PRECONDITION", false},
	{sso.ErrDomainTaken, http.StatusConflict, "ALREADY_EXISTS", false},
	{sso.ErrDomainUnproven, http.StatusConflict, "FAILED_PRECONDITION", false},
	{sso.ErrDomainNotFound, http.StatusNotFound, "NOT_FOUND", false},
	{sso.ErrTooManyDomains, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrTooManyDomains, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrPublicMemberNotAllowed, http.StatusForbidden, "ORG_POLICY_VIOLATION", false},
	{iam.ErrMemberDomainNotAllowed, http.StatusForbidden, "ORG_POLICY_VIOLATION", true},
	{iam.ErrKeyCreationDisabled, http.StatusForbidden, "ORG_POLICY_VIOLATION", false},
	{iam.ErrInvalidIssuer, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrInvalidSubjectPattern, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrSubjectTooBroad, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrInvalidConditions, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{iam.ErrTooManyTrusts, http.StatusConflict, "FAILED_PRECONDITION", false},
	{iam.ErrTrustNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{iam.ErrInvalidClusterJWKS, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{workloadsSvc.ErrWorkloadIdentityDisabled, http.StatusConflict, "FAILED_PRECONDITION", false},
	{workloadsSvc.ErrWorkloadNameTaken, http.StatusConflict, betoerrors.CodeConflict, false},
	{projectsSvc.ErrProjectNameTaken, http.StatusConflict, betoerrors.CodeConflict, false},
	{projectsSvc.ErrProjectLimit, http.StatusConflict, "PROJECT_LIMIT_REACHED", false},
	{projectsSvc.ErrInvalidProjectName, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{projectsSvc.ErrInvalidOrganization, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{projectsSvc.ErrProjectNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{projectsSvc.ErrInvalidProjectLimits, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{projectsSvc.ErrLimitsBelowUsage, http.StatusConflict, "FAILED_PRECONDITION", false},
	{projectsSvc.ErrLimitsUpdateInProgress, http.StatusConflict, betoerrors.CodeConflict, false},

	{billingDomain.ErrInvalidMonth, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{billingDomain.ErrUnknownCurrency, http.StatusNotFound, "UNKNOWN_CURRENCY", false},
	// La raison (taille, montant négatif…) aide l'équipe à corriger la grille.
	{billingDomain.ErrInvalidCatalog, http.StatusBadRequest, "INVALID_CATALOG", true},
	{billingDomain.ErrTopupsClosed, http.StatusConflict, "TOPUPS_CLOSED", false},
	{billingDomain.ErrTopupAmount, http.StatusBadRequest, "TOPUP_AMOUNT", true},
	{billingDomain.ErrInvalidPhone, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{billingDomain.ErrInvalidOperator, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{billingDomain.ErrPaymentsDisabled, http.StatusServiceUnavailable, "PAYMENTS_DISABLED", false},
	{billingDomain.ErrChargingDisabled, http.StatusConflict, "BILLING_NOT_ACTIVE", false},
	{billingDomain.ErrTopupNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{billingDomain.ErrTopupPending, http.StatusConflict, "TOPUP_PENDING", false},
	{billingDomain.ErrInvalidCursor, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	// Le code du prestataire aide à comprendre le refus (solde insuffisant…).
	{billingDomain.ErrPaymentRefused, http.StatusBadGateway, "PAYMENT_REFUSED", true},
	{billingDomain.ErrReasonRequired, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{billingDomain.ErrInvalidAdminAmount, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{billingDomain.ErrOrganizationNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{billingDomain.ErrAccountSuspended, http.StatusPaymentRequired, "ACCOUNT_SUSPENDED", false},
	{billingDomain.ErrBillingCheckUnavailable, http.StatusServiceUnavailable, "BILLING_CHECK_UNAVAILABLE", false},
	{billingDomain.ErrInvalidProfile, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{billingDomain.ErrDocumentNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{billingDomain.ErrInvalidBudget, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{billingDomain.ErrPlanNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{billingDomain.ErrInvalidPayment, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{billingDomain.ErrInsufficientFunds, http.StatusPaymentRequired, "INSUFFICIENT_FUNDS", false},
	{domainsalesDomain.ErrInvalidDomain, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{domainsalesDomain.ErrNotAvailable, http.StatusConflict, "DOMAIN_NOT_AVAILABLE", false},
	{domainsalesDomain.ErrPremium, http.StatusConflict, "DOMAIN_PREMIUM", false},
	{domainsalesDomain.ErrNoPrice, http.StatusServiceUnavailable, "DOMAIN_NO_PRICE", false},
	{domainsalesDomain.ErrInvalidContact, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{domainsalesDomain.ErrOrderNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{domainsalesDomain.ErrNotActive, http.StatusConflict, "DOMAIN_NOT_ACTIVE", false},
	{domainsalesDomain.ErrSalesDisabled, http.StatusServiceUnavailable, "DOMAIN_SALES_DISABLED", false},
	{domainsalesDomain.ErrPriceChanged, http.StatusConflict, "DOMAIN_PRICE_CHANGED", false},
	{domainsalesDomain.ErrUnavailable, http.StatusServiceUnavailable, "DOMAIN_SERVICE_UNAVAILABLE", false},
	{billingDomain.ErrInvalidPlan, http.StatusBadRequest, "INVALID_PLAN", true},
	{billingDomain.ErrPlanNeedsBilling, http.StatusConflict, "PLAN_NEEDS_BILLING", false},
	// Les projets en cause aident le client à libérer de la place.
	{billingDomain.ErrPlanBelowUsage, http.StatusConflict, "PLAN_BELOW_USAGE", true},
}

// classifyError : statut, code et message sûr à exposer.
func classifyError(err error) (int, ErrorBody) {
	body := func(code, msg string) ErrorBody { return ErrorBody{Code: code, Message: msg, Error: msg} }

	var bindErr tonic.BindError
	var bindErrPtr *tonic.BindError
	if errors.As(err, &bindErr) || errors.As(err, &bindErrPtr) {
		msg := err.Error()
		if bindErrPtr != nil {
			bindErr = *bindErrPtr
		}
		// Lignes du validator, sans le préfixe de tonic : le middleware de
		// traduction les reformule dans la langue de l'utilisateur.
		if ve := bindErr.ValidationErrors(); len(ve) > 0 {
			msg = ve.Error()
		}
		return http.StatusBadRequest, body(betoerrors.CodeInvalidInput, msg)
	}
	var typed *betoerrors.Error
	if errors.As(err, &typed) {
		status, _ := betoerrors.ToHTTPError(typed)
		return status, body(typed.Code, typed.Message)
	}
	for _, m := range sentinelErrors {
		if errors.Is(err, m.err) {
			msg := m.err.Error()
			if m.detailed {
				msg = err.Error()
			}
			if m.err == gorm.ErrRecordNotFound {
				msg = "not found"
			}
			return m.status, body(m.code, msg)
		}
	}
	return http.StatusInternalServerError, body(betoerrors.CodeInternal, "internal server error")
}

// newErrorHook : hook tonic global. Les 5xx sont journalisés avec la cause
// réelle, qui n'est jamais renvoyée au client.
func newErrorHook(logger *zap.SugaredLogger) tonic.ErrorHook {
	return func(c *gin.Context, err error) (int, interface{}) {
		status, body := classifyError(err)
		if status >= http.StatusInternalServerError && logger != nil {
			logger.Errorw("request failed",
				"method", c.Request.Method, "route", c.FullPath(), "status", status, "error", err)
		}
		return status, body
	}
}
