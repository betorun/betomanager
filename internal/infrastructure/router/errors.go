package router

import (
	"errors"
	sitesDomain "github.com/thekrauss/kubemanager/internal/modules/sites/domain"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
	"net/http"

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
	{iam.ErrMFANotConfigured, http.StatusConflict, "FAILED_PRECONDITION", false},
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
	{billingDomain.ErrInvalidProfile, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{billingDomain.ErrDocumentNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
	{billingDomain.ErrInvalidBudget, http.StatusBadRequest, betoerrors.CodeInvalidInput, false},
	{billingDomain.ErrPlanNotFound, http.StatusNotFound, betoerrors.CodeNotFound, false},
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
