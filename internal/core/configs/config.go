package configs

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/thekrauss/beto-shared/pkg/logger"
)

type GlobalConfig struct {
	// Env : "production" active les réglages par défaut et le refus de démarrer
	// d'une configuration incomplète (production.go). Variable KUBEMANAGER_ENV.
	Env         string         `mapstructure:"env"`
	ServiceName string         `mapstructure:"service_name"`
	ProjectID   string         `mapstructure:"project_id"`
	Logger      logger.Config  `mapstructure:"logger"`
	Server      ServerConfig   `mapstructure:"server"`
	Database    DBConfig       `mapstructure:"database"`
	Redis       RedisConfig    `mapstructure:"redis"`
	RabbitMQ    RabbitConfig   `mapstructure:"rabbitmq"`
	Metrics     MetricsConfig  `mapstructure:"metrics"`
	Tracing     TracingConfig  `mapstructure:"tracing"`
	Temporal    TemporalConfig `mapstructure:"temporal"`
	Mail        MailConfig     `mapstructure:"mail"`
	Swagger     SwaggerConfig  `mapstructure:"swagger"`
	JWT         JWTConfig      `mapstructure:"jwt"`
	Frontend    FrontendConfig `mapstructure:"frontend"`
	// Social : connexion avec Google / GitHub (applications OAuth de betorun).
	Social       SocialConfig       `mapstructure:"social"`
	// SSO : connexion d'entreprise par organisation (iam.md §9.2).
	SSO SSOConfig `mapstructure:"sso"`
	Roles        RolesConfig        `mapstructure:"roles"`
	Kubernetes   KubernetesConfig   `mapstructure:"kubernetes"`
	Vps          Vps                `mapstructure:"vps"`
	Multitenancy MultitenancyConfig `mapstructure:"multitenancy"`
	Centrifugo   CentrifugoConfig   `mapstructure:"centrifugo"`
	Keycloak     KeycloakConfig     `mapstructure:"keycloak"`
	Audit        AuditConfig        `mapstructure:"audit"`
	RateLimit    RateLimitConfig    `mapstructure:"rate_limit"`
	ObjectStore  ObjectStoreConfig  `mapstructure:"object_store"`
	Sites        SitesConfig        `mapstructure:"sites"`
	Builds       BuildsConfig       `mapstructure:"builds"`
	IAM          IAMConfig          `mapstructure:"iam"`
	// GitHubApp : connexion GitHub des conteneurs (dépôts privés, déploiement
	// à chaque push). Désactivée tant qu'elle est incomplète.
	GitHubApp GitHubAppConfig `mapstructure:"github_app"`
	// LLM : modèles de langage (Copilot). Désactivé par défaut.
	LLM LLMConfig `mapstructure:"llm"`
	// Databases : bases Postgres gérées (CloudNativePG).
	Databases DatabasesConfig `mapstructure:"databases"`
	// Billing : facturation (ADR 0012). Aucun montant ici : les grilles
	// sont en base, modifiables depuis l'administration.
	Billing BillingConfig `mapstructure:"billing"`
	// Domains : revente de noms de domaine (registraire). Prod :
	// KUBEMANAGER_DOMAINS_OVH_*.
	Domains DomainsConfig `mapstructure:"domains"`
}

// DomainsConfig : registraires.
type DomainsConfig struct {
	OVH OVHConfig `mapstructure:"ovh"`
}

// OVHConfig : API OVH (https://eu.api.ovh.com/createToken/). Clé limitée à
// /order/* et /domain/*. Vide : revente de domaines fermée.
type OVHConfig struct {
	// Endpoint : défaut https://eu.api.ovh.com/1.0.
	Endpoint          string `mapstructure:"endpoint"`
	ApplicationKey    string `mapstructure:"application_key"`
	ApplicationSecret string `mapstructure:"application_secret"`
	ConsumerKey       string `mapstructure:"consumer_key"`
	// Subsidiary : filiale OVH des commandes (ex. FR), qui fixe la devise.
	Subsidiary string `mapstructure:"subsidiary"`
	// OrderingEnabled : achats réels permis (payés par le moyen de paiement
	// par défaut du compte OVH). Faux : recherche et prix seulement.
	OrderingEnabled bool `mapstructure:"ordering_enabled"`
}

// BillingConfig : rail de paiement des recharges.
type BillingConfig struct {
	XYKPay XYKPayConfig `mapstructure:"xykpay"`
	// Issuer : entité qui facture (factures et reçus). Nom et adresse vides :
	// aucun document émis. Prod : KUBEMANAGER_BILLING_ISSUER_*.
	Issuer BillingIssuerConfig `mapstructure:"issuer"`
}

// BillingIssuerConfig : mentions légales des factures.
type BillingIssuerConfig struct {
	Name    string `mapstructure:"name"`
	Address string `mapstructure:"address"`
	TaxID   string `mapstructure:"tax_id"`
	Country string `mapstructure:"country"`
	// VATRateBP : taux de TVA en points de base (1890 = 18,90 %) ; les prix
	// de la grille sont TTC.
	VATRateBP int `mapstructure:"vat_rate_bp"`
	// PaymentInstructions : comment régler une facture postpayée (RIB, banque…),
	// imprimé sur la facture.
	PaymentInstructions string `mapstructure:"payment_instructions"`
	// InvoicePrefix / ReceiptPrefix : défaut BTR / REC.
	InvoicePrefix string `mapstructure:"invoice_prefix"`
	ReceiptPrefix string `mapstructure:"receipt_prefix"`
}

// XYKPayConfig : compte marchand betorun chez XYKPay. Vide : recharges
// fermées. Prod : KUBEMANAGER_BILLING_XYKPAY_*.
type XYKPayConfig struct {
	// BaseURL : ex. https://pay.sykedu.com (sans /api/v1).
	BaseURL string `mapstructure:"base_url"`
	// APIKey : clé secrète (xyk_sk_…).
	APIKey string `mapstructure:"api_key"`
	// WebhookSecret : secret de signature des webhooks (XYK-Signature).
	WebhookSecret string `mapstructure:"webhook_secret"`
}

// DatabasesConfig : bases gérées.
type DatabasesConfig struct {
	// StorageClass : classe des disques (vide : celle par défaut du cluster).
	StorageClass string `mapstructure:"storage_class"`
	// Backup : sauvegardes continues (plugin Barman Cloud) vers un stockage
	// S3 JOIGNABLE DEPUIS LE CLUSTER (les pods des bases y écrivent).
	// Désactivées tant que Endpoint ou Bucket est vide.
	Backup DatabaseBackupConfig `mapstructure:"backup"`
	// ImportFetchImage : image avec curl qui télécharge un dump à importer
	// (défaut curlimages/curl, version figée).
	ImportFetchImage string `mapstructure:"import_fetch_image"`
	// ImportMaxBytes : taille maximale d'un dump (défaut 2 Gio).
	ImportMaxBytes int64 `mapstructure:"import_max_bytes"`
}

// DatabaseBackupConfig : destination des sauvegardes des bases.
type DatabaseBackupConfig struct {
	// Endpoint : URL S3 complète, ex. https://s3.eu-central-003.backblazeb2.com
	// ou https://storage.betorun.app (SeaweedFS de la plateforme).
	Endpoint string `mapstructure:"endpoint"`
	Bucket   string `mapstructure:"bucket"`
	Region   string `mapstructure:"region"`
	// AccessKey / SecretKey : secrets. Prod : KUBEMANAGER_DATABASES_BACKUP_*.
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	// Retention : durée de conservation, ex. 7d, 4w, 3m (défaut 7d).
	Retention string `mapstructure:"retention"`
	// Schedule : sauvegarde complète, cron à 6 champs avec secondes
	// (défaut « 0 0 3 * * * », chaque nuit à 3 h UTC).
	Schedule string `mapstructure:"schedule"`
}

// LLMConfig : accès aux modèles de langage (internal/infrastructure/llm,
// docs/adr/0008-llm-provider-layer.md). Deux emplacements fixes plutôt
// qu'une liste : chaque champ reste réglable par une variable
// KUBEMANAGER_LLM_PRIMARY_* / KUBEMANAGER_LLM_FALLBACK_* (la prod se règle
// par l'environnement, et viper ne lie pas les listes).
type LLMConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Primary est obligatoire si Enabled ; Fallback est facultatif (BaseURL vide = absent).
	Primary  LLMProviderConfig `mapstructure:"primary"`
	Fallback LLMProviderConfig `mapstructure:"fallback"`
	// DailyBudgetMicroUSD : plafond de dépense par organisation et par jour
	// (UTC), en micro-dollars. 2 000 000 = 2 $.
	DailyBudgetMicroUSD int64 `mapstructure:"daily_budget_micro_usd"`
	// BreakerFailures / BreakerCooldown : disjoncteur par fournisseur.
	BreakerFailures int           `mapstructure:"breaker_failures"`
	BreakerCooldown time.Duration `mapstructure:"breaker_cooldown"`
}

// LLMProviderConfig : un fournisseur compatible OpenAI (OpenAI, vLLM, Groq,
// Gemini, Mistral…).
type LLMProviderConfig struct {
	// Name : nom court et stable (labels de métriques), ex. "qwen", "groq".
	Name    string `mapstructure:"name"`
	BaseURL string `mapstructure:"base_url"`
	// APIKey : secret. Prod : KUBEMANAGER_LLM_PRIMARY_API_KEY.
	APIKey  string        `mapstructure:"api_key"`
	Model   string        `mapstructure:"model"`
	Timeout time.Duration `mapstructure:"timeout"`
	// MaxTokensField : "max_tokens" (défaut) ou "max_completion_tokens".
	MaxTokensField string `mapstructure:"max_tokens_field"`
	// ExtraBodyJSON : objet JSON fusionné dans chaque requête, ex.
	// {"chat_template_kwargs":{"enable_thinking":false}} pour Qwen sur vLLM.
	ExtraBodyJSON string `mapstructure:"extra_body_json"`
	// Prix en micro-dollars par million de jetons (budget). Un modèle
	// auto-hébergé reçoit un prix interne, sinon rien n'est plafonné.
	InputMicroUSDPerMTok  int64 `mapstructure:"input_micro_usd_per_mtok"`
	OutputMicroUSDPerMTok int64 `mapstructure:"output_micro_usd_per_mtok"`
}

// GitHubAppConfig : application GitHub de betorun.
// Les secrets viennent de l'environnement ; la clé privée d'un fichier, hors
// du code et de la base.
type GitHubAppConfig struct {
	AppID          string `mapstructure:"app_id"`
	Slug           string `mapstructure:"slug"`
	PrivateKeyFile string `mapstructure:"private_key_file"`
	WebhookSecret  string `mapstructure:"webhook_secret"`
	ClientID       string `mapstructure:"client_id"`
	ClientSecret   string `mapstructure:"client_secret"`
	// APIURL et WebURL : surchargeables pour les tests (ou GitHub Enterprise).
	// WebURL sert aussi à construire l'adresse de clonage des dépôts : c'est
	// une valeur de l'opérateur, elle échappe au contrôle des URL saisies par
	// les utilisateurs (builds/domain.NormalizeGitURL).
	APIURL string `mapstructure:"api_url"`
	WebURL string `mapstructure:"web_url"`
}

// Complete : tous les réglages obligatoires sont renseignés.
func (c GitHubAppConfig) Complete() bool {
	return c.AppID != "" && c.Slug != "" && c.PrivateKeyFile != "" && c.WebhookSecret != "" && c.ClientID != "" && c.ClientSecret != ""
}

// WithDefaults : adresses de github.com si elles ne sont pas renseignées.
func (c GitHubAppConfig) WithDefaults() GitHubAppConfig {
	if c.APIURL == "" {
		c.APIURL = "https://api.github.com"
	}
	if c.WebURL == "" {
		c.WebURL = "https://github.com"
	}
	return c
}

// IAMConfig : secrets de l'IAM (docs/architecture/iam.md).
type IAMConfig struct {
	// APIKeyPepper : secret serveur des clés API v2 (HMAC). ≥ 32 octets, hors
	// base de données (env KUBEMANAGER_IAM_API_KEY_PEPPER en prod). Le changer
	// invalide toutes les clés existantes.
	APIKeyPepper string `mapstructure:"api_key_pepper"`
	// PlatformAdmins / PlatformSupport : emails du staff betorun. Source de
	// vérité des rôles roles/platform.admin et roles/platform.support : un
	// email retiré perd son rôle à la réconciliation suivante (≤ 10 min).
	PlatformAdmins  []string `mapstructure:"platform_admins"`
	PlatformSupport []string `mapstructure:"platform_support"`
	// STSIssuer : URL publique du STS (émetteur des jetons machine), c'est-à-
	// dire <URL publique de l'API>/kmanager/v1/iam. Le endpoint de jeton est
	// <STSIssuer>/token et les clés publiques <STSIssuer>/.well-known/jwks.json.
	STSIssuer string `mapstructure:"sts_issuer"`
	// STSKeyEncryptionKey : 32 octets en base64 ; chiffre (AES-256-GCM) les
	// clés de signature du STS stockées en base. Secret (env
	// KUBEMANAGER_IAM_STS_KEY_ENCRYPTION_KEY en prod), remplaçable par un KMS.
	STSKeyEncryptionKey string `mapstructure:"sts_key_encryption_key"`
	// MFAACRValues : valeurs du claim acr (Keycloak, mapping ACR → niveau
	// d'authentification du realm) qui attestent une double authentification,
	// ex. ["gold"]. Vide : MFA non configuré (pas de step-up, organisation
	// ne peut pas exiger le MFA).
	MFAACRValues []string `mapstructure:"mfa_acr_values"`
	// StepUpMaxAge : fraîcheur exigée de l'authentification MFA pour les
	// actions sensibles (défaut 10 min).
	StepUpMaxAge time.Duration `mapstructure:"step_up_max_age"`
}

// ObjectStoreConfig : stockage S3-compatible (SeaweedFS en dev) des sites statiques.
type ObjectStoreConfig struct {
	Endpoint  string `mapstructure:"endpoint"` // host:port, sans schéma
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	Bucket    string `mapstructure:"bucket"`
	Region    string `mapstructure:"region"`
	UseSSL    bool   `mapstructure:"use_ssl"`
}

// SitesConfig : hébergement de sites statiques.
type SitesConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// BaseDomain : chaque site est servi sur <subdomain>.<base_domain>.
	BaseDomain string `mapstructure:"base_domain"`
	// EdgeTarget : cible CNAME à donner aux utilisateurs pour un domaine perso.
	EdgeTarget string `mapstructure:"edge_target"`
	// Limites d'une archive de déploiement (0 = défaut du service).
	MaxArchiveBytes   int64 `mapstructure:"max_archive_bytes"`
	MaxExtractedBytes int64 `mapstructure:"max_extracted_bytes"`
	MaxFiles          int   `mapstructure:"max_files"`
	// EdgeIngress : static-edge dans le cluster (production). Chaque domaine
	// personnalisé vérifié y reçoit un Ingress et son certificat.
	EdgeIngress EdgeIngressConfig `mapstructure:"edge_ingress"`
}

// EdgeIngressConfig : Ingress des domaines personnalisés des sites, vers
// static-edge (deploy/k8s/static-edge.yaml).
type EdgeIngressConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Namespace et Service de static-edge (défauts : betorun-edge, static-edge:80).
	Namespace string `mapstructure:"namespace"`
	Service   string `mapstructure:"service"`
	Port      int    `mapstructure:"port"`
	// Issuer : ClusterIssuer cert-manager en HTTP-01 (défaut letsencrypt-prod).
	Issuer string `mapstructure:"issuer"`
}

// WithDefaults : valeurs de deploy/k8s/static-edge.yaml pour les champs vides.
func (c EdgeIngressConfig) WithDefaults() EdgeIngressConfig {
	if c.Namespace == "" {
		c.Namespace = "betorun-edge"
	}
	if c.Service == "" {
		c.Service = "static-edge"
	}
	if c.Port == 0 {
		c.Port = 80
	}
	if c.Issuer == "" {
		c.Issuer = "letsencrypt-prod"
	}
	return c
}

// BuildsConfig : construction d'images depuis le code et registre de la
// plateforme. Désactivé tant que le registre n'est pas configuré.
type BuildsConfig struct {
	Enabled   bool   `mapstructure:"enabled"`
	Namespace string `mapstructure:"namespace"`
	// BuildkitImage : image BuildKit sans root, en version figée.
	BuildkitImage string `mapstructure:"buildkit_image"`
	// RegistryHost : registre vu par les Jobs et les nœuds (ex. registry.betorun.app).
	RegistryHost string `mapstructure:"registry_host"`
	// RegistryInsecure : registre en HTTP, développement uniquement.
	RegistryInsecure bool `mapstructure:"registry_insecure"`
	// RegistryAPIURL : registre vu par l'API et le worker (taille et ménage des
	// images) ; défaut : https://<registry_host> (http si registry_insecure).
	RegistryAPIURL string `mapstructure:"registry_api_url"`
	// KeepImages : images gardées par dépôt, en plus de celles en ligne (défaut 5).
	KeepImages int `mapstructure:"keep_images"`
	// ProjectImageQuota : espace d'images par projet, quantité Kubernetes
	// (défaut 5Gi ; "0" : sans quota).
	ProjectImageQuota string `mapstructure:"project_image_quota"`
	RegistryService   string `mapstructure:"registry_service"`
	TokenIssuer       string `mapstructure:"token_issuer"`
	// Certificat et clé ECDSA P-256 des jetons du registre (rootcertbundle
	// du registre = ce certificat).
	TokenCertFile string `mapstructure:"token_cert_file"`
	TokenKeyFile  string `mapstructure:"token_key_file"`
	// Secret : dérive les identifiants du registre et signe les URL des
	// archives (≥ 32 octets). Le changer invalide les Secrets de pull existants.
	Secret string `mapstructure:"secret"`
	// PublicAPIURL : URL de l'API vue depuis les pods (…/kmanager/v1).
	PublicAPIURL    string        `mapstructure:"public_api_url"`
	MaxArchiveBytes int64         `mapstructure:"max_archive_bytes"`
	MaxConcurrent   int64         `mapstructure:"max_concurrent"`
	Timeout         time.Duration `mapstructure:"timeout"`
	CPURequest      string        `mapstructure:"cpu_request"`
	CPULimit        string        `mapstructure:"cpu_limit"`
	MemoryRequest   string        `mapstructure:"memory_request"`
	MemoryLimit     string        `mapstructure:"memory_limit"`
	DiskLimit       string        `mapstructure:"disk_limit"`
	// EgressAllowCIDRs : adresses privées qu'un build peut joindre (registre
	// et API s'ils sont sur un réseau privé).
	EgressAllowCIDRs []string `mapstructure:"egress_allow_cidrs"`
	// RuntimeClass : runtime isolé des builds en production (gVisor, Kata).
	RuntimeClass string            `mapstructure:"runtime_class"`
	NodeSelector map[string]string `mapstructure:"node_selector"`
}

// WithDefaults : réglages non renseignés remplacés par leur valeur par défaut.
func (c BuildsConfig) WithDefaults() BuildsConfig {
	str := func(v *string, def string) {
		if *v == "" {
			*v = def
		}
	}
	str(&c.Namespace, "betorun-builds")
	str(&c.BuildkitImage, "moby/buildkit:v0.23.2-rootless")
	str(&c.RegistryService, "betorun-registry")
	str(&c.TokenIssuer, "betorun")
	str(&c.CPURequest, "250m")
	str(&c.CPULimit, "2")
	str(&c.MemoryRequest, "512Mi")
	str(&c.MemoryLimit, "4Gi")
	str(&c.DiskLimit, "10Gi")
	if c.MaxArchiveBytes == 0 {
		c.MaxArchiveBytes = 200 << 20
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 2
	}
	if c.RegistryAPIURL == "" && c.RegistryHost != "" {
		scheme := "https://"
		if c.RegistryInsecure {
			scheme = "http://"
		}
		c.RegistryAPIURL = scheme + c.RegistryHost
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Minute
	}
	return c
}

// AuditConfig : contrôle le sampling des logs d'audit (fix P1).
// Les events de sécurité critiques (failed/revoked/permission_denied/creates)
// sont TOUJOURS loggés quel que soit le sampling.
type AuditConfig struct {
	// SampleRate : 1 sur N events "normaux" (success, apikey_used) est loggé.
	// 0 ou 1 → pas de sampling, tout est loggé (défaut en dev).
	// Ex : 100 → 1% des success. En prod haut trafic, recommandé : 50-1000.
	SampleRate int `mapstructure:"sample_rate"`
}

type KeycloakConfig struct {
	Enabled           bool   `mapstructure:"enabled"`
	IssuerURL         string `mapstructure:"issuer_url"`
	JWKSURL           string `mapstructure:"jwks_url"`
	Audience          string `mapstructure:"audience"`
	ClientID          string `mapstructure:"client_id"`
	ClientSecret      string `mapstructure:"client_secret"`
	Realm             string `mapstructure:"realm"`
	AdminBaseURL      string `mapstructure:"admin_base_url"`
	AdminRealm        string `mapstructure:"admin_realm"`
	AdminClientID     string `mapstructure:"admin_client_id"`
	AdminClientSecret string `mapstructure:"admin_client_secret"`
	StrictAudience    bool   `mapstructure:"strict_audience"`
	// AcceptUserTokens : accepter un jeton Keycloak en Bearer. Faux par
	// défaut : les humains n'utilisent que des sessions betorun
	// (docs/architecture/auth.md §11). Vrai seulement pour les tests locaux.
	AcceptUserTokens  bool          `mapstructure:"accept_user_tokens"`
	HTTPTimeout       time.Duration `mapstructure:"http_timeout"`
	SocialRedirectURI string        `mapstructure:"social_redirect_uri"`
	// AuthClientID / AuthClientSecret : client confidentiel réservé au
	// backend, qui vérifie les mots de passe (docs/architecture/auth.md §5).
	// Jamais exposé : le navigateur et la CLI ne parlent qu'à l'API betorun.
	AuthClientID     string `mapstructure:"auth_client_id"`
	AuthClientSecret string `mapstructure:"auth_client_secret"`
}

type JWTConfig struct {
	Secret            string        `mapstructure:"secret"`
	RefreshSecret     string        `mapstructure:"refresh_secret"`
	AccessExpiration  time.Duration `mapstructure:"access_expiration"`
	RefreshExpiration time.Duration `mapstructure:"refresh_expiration"`
}

type RateLimitConfig struct {
	Enabled bool          `mapstructure:"enabled"`
	Limit   int           `mapstructure:"limit"` // max requests per Window
	Burst   int           `mapstructure:"burst"` // legacy, ignored (fixed window)
	Window  time.Duration `mapstructure:"window"`
}

type MailConfig struct {
	SMTPHost     string `mapstructure:"smtp_host"`
	SMTPPort     int    `mapstructure:"smtp_port"`
	SMTPUser     string `mapstructure:"smtp_user"`
	SMTPPassword string `mapstructure:"smtp_password"`
	FromEmail    string `mapstructure:"from_email"`
	FromName     string `mapstructure:"from_name"`
	Enabled      bool   `mapstructure:"enabled"`
}

type ServerConfig struct {
	GRPCPort        int           `mapstructure:"grpc_port"`
	HTTPPort        int           `mapstructure:"http_port"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	AllowedOrigins  []string      `mapstructure:"allowed_origins"`
	// TrustedProxies : adresses ou CIDR des proxies dont les en-têtes
	// X-Forwarded-For sont crus (ingress, BFF de la console). Vide : boucle
	// locale seulement. Sinon un client choisirait son IP et contournerait
	// les limites de tentatives. Derrière Caddy en conteneur : les IP fixes de
	// Caddy et de la console sur le réseau interne (docker-compose.prod.yaml),
	// jamais 0.0.0.0/0. Le rate limit et les limites de connexion en dépendent :
	// sans cette liste, tous les clients partagent l'IP de Caddy.
	TrustedProxies []string `mapstructure:"trusted_proxies"`
}

type DBConfig struct {
	Driver   string `mapstructure:"driver"`
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	Name     string `mapstructure:"name"`
	SSLMode  string `mapstructure:"sslmode"`
	// Pool par processus (0 : défauts de database.PoolDefaults). La somme des
	// processus doit rester sous max_connections de Postgres.
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"`
	MigrationsPath  string        `mapstructure:"migrations_path"`
	// SkipMigrations : ce processus n'applique pas les migrations. En
	// production, seule l'API les lance et le worker démarre après elle.
	SkipMigrations bool `mapstructure:"skip_migrations"`
}

type CentrifugoConfig struct {
	Enabled         bool     `mapstructure:"enabled"`
	URL             string   `mapstructure:"url"`
	APIURL          string   `mapstructure:"api_url"`
	APIKey          string   `mapstructure:"api_key"`
	TokenHMACSecret string   `mapstructure:"token_hmac_secret"`
	AllowedOrigins  []string `mapstructure:"allowed_origins"`
	AdminEnabled    bool     `mapstructure:"admin_enabled"`
	AdminPassword   string   `mapstructure:"admin_password"`
	AdminSecret     string   `mapstructure:"admin_secret"`
}

type MultitenancyConfig struct {
	BaseDomain     string `mapstructure:"base_domain"`
	AllowCustomDom bool   `mapstructure:"allow_custom_dom"`
}

type RedisConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type SwaggerConfig struct {
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
}

type RabbitConfig struct {
	URL      string `mapstructure:"url"`
	Exchange string `mapstructure:"exchange"`
	Queue    string `mapstructure:"queue"`
}

type TemporalConfig struct {
	Host              string        `mapstructure:"host"`
	Namespace         string        `mapstructure:"namespace"`
	TaskQueue         string        `mapstructure:"task_queue"`
	ConnectionTimeout time.Duration `mapstructure:"connection_timeout"`
	// Bornes du worker (0 : défauts ci-dessous). Sans borne, le SDK accepte
	// 1000 activités simultanées et garde 10 000 workflows en cache.
	MaxConcurrentActivities    int           `mapstructure:"max_concurrent_activities"`
	MaxConcurrentWorkflowTasks int           `mapstructure:"max_concurrent_workflow_tasks"`
	StickyCacheSize            int           `mapstructure:"sticky_cache_size"`
	WorkerStopTimeout          time.Duration `mapstructure:"worker_stop_timeout"`
}

// Défauts du worker Temporal, prévus pour une petite machine.
const (
	DefaultMaxConcurrentActivities    = 6
	DefaultMaxConcurrentWorkflowTasks = 8
	DefaultStickyCacheSize            = 256
	DefaultWorkerStopTimeout          = 30 * time.Second
)

// WithWorkerDefaults : réglages du worker non renseignés remplacés par leur
// valeur par défaut.
func (c TemporalConfig) WithWorkerDefaults() TemporalConfig {
	if c.MaxConcurrentActivities <= 0 {
		c.MaxConcurrentActivities = DefaultMaxConcurrentActivities
	}
	if c.MaxConcurrentWorkflowTasks <= 0 {
		c.MaxConcurrentWorkflowTasks = DefaultMaxConcurrentWorkflowTasks
	}
	if c.StickyCacheSize <= 0 {
		c.StickyCacheSize = DefaultStickyCacheSize
	}
	if c.WorkerStopTimeout <= 0 {
		c.WorkerStopTimeout = DefaultWorkerStopTimeout
	}
	return c
}

type MetricsConfig struct {
	PrometheusPort int `mapstructure:"prometheus_port"`
	// ListenAddr : si renseigné, /metrics est exposé sur un listener dédié
	// (ex: ":9100") au lieu de l'engine public. Permet une NetworkPolicy
	// serrée "monitoring namespace → :9100" sans exposer les metrics aux
	// utilisateurs finaux.
	ListenAddr string `mapstructure:"listen_addr"`
}

type TracingConfig struct {
	Exporter       string `mapstructure:"exporter"`
	JaegerEndpoint string `mapstructure:"jaeger_endpoint"`
	// SampleRatio : ratio de sampling OTEL (0.0 → 1.0). Défaut 1.0 en dev,
	// recommandé 0.01 (1%) en prod pour économiser Jaeger + CPU.
	SampleRatio float64 `mapstructure:"sample_ratio"`
}

type Vps struct {
	// AdressIp : IP publique du point d'entrée des conteneurs (URL
	// <release>-<projet>.<ip>.sslip.io). Clé vps.vps_ip (env
	// KUBEMANAGER_VPS_VPS_IP) ; vide : conteneurs sans URL publique.
	AdressIp string `mapstructure:"vps_ip"`
}

// SocialConfig : un fournisseur n'est proposé que si son client_id est
// renseigné. Les adresses ont des valeurs par défaut (réglables pour les tests).
type SocialConfig struct {
	Google SocialProviderConfig `mapstructure:"google"`
	GitHub SocialProviderConfig `mapstructure:"github"`
}

// SSOConfig : fournisseurs d'identité des organisations.
type SSOConfig struct {
	// AllowInsecureIssuers : accepter http et les adresses internes
	// (Keycloak local). Jamais en production : protection SSRF.
	AllowInsecureIssuers bool `mapstructure:"allow_insecure_issuers"`
}

type SocialProviderConfig struct {
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`
	AuthURL      string `mapstructure:"auth_url"`
	TokenURL     string `mapstructure:"token_url"`
	APIURL       string `mapstructure:"api_url"`
	JWKSURL      string `mapstructure:"jwks_url"`
	Issuer       string `mapstructure:"issuer"`
}

type FrontendConfig struct {
	BaseURL string `mapstructure:"url"`
}

type RolesConfig struct {
	PlatformAdmin string `mapstructure:"platform_admin"`
}

type KubernetesConfig struct {
	// IngressTarget : nom ou adresse vers laquelle les domaines personnalisés
	// doivent pointer (défaut : vps_ip).
	IngressTarget string `mapstructure:"ingress_target"`
	// DomainDNSServer : résolveurs (hôte:port, séparés par des virgules) des
	// vérifications de domaine ; vide : 1.1.1.1 puis 8.8.8.8 ; « system » :
	// celui de la machine (cache négatif possible).
	DomainDNSServer string `mapstructure:"domain_dns_server"`
	// CustomDomainInsecure : pas de certificat pour les domaines (développement).
	CustomDomainInsecure bool `mapstructure:"custom_domain_insecure"`

	KubeConfigPath string `mapstructure:"kubeconfig_path"`
	// ClusterID : identifiant (celui de son watcher) du cluster où tournent
	// les workloads. Vide : identité des workloads désactivée.
	ClusterID string `mapstructure:"cluster_id"`
	// IngressClass : classe d'ingress des conteneurs exposés (défaut nginx ;
	// traefik sur un k3s/k3d nu, obligatoire en production sous k3s).
	IngressClass string `mapstructure:"ingress_class"`
	// IngressNamespace : espace de noms du contrôleur d'ingress, seul autorisé
	// par la NetworkPolicy à joindre les conteneurs (défaut ingress-nginx ;
	// kube-system pour le traefik de k3s).
	IngressNamespace string `mapstructure:"ingress_namespace"`
}

// EnvPrefix est le préfixe utilisé pour les variables d'environnement (ex: KUBEMANAGER_DATABASE_HOST).
const EnvPrefix = "KUBEMANAGER"

// DefaultShutdownTimeout : arrêt gracieux si server.shutdown_timeout est absent
// (sans cela, les requêtes en cours étaient coupées immédiatement).
const DefaultShutdownTimeout = 15 * time.Second

var AppConfig GlobalConfig

func Load(path string) (*GlobalConfig, error) {
	v := viper.New()
	v.SetConfigType("yaml")

	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("config")
		v.AddConfigPath("./internal/core/configs")
		v.AddConfigPath(".")
		v.AddConfigPath("..")
		v.AddConfigPath("./config")
	}

	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv ne sert à Unmarshal que pour les clés déjà connues de
	// viper (présentes dans le fichier) : sans liaison explicite, une
	// variable comme KUBEMANAGER_IAM_API_KEY_PEPPER serait ignorée si le
	// fichier n'a pas de section iam (cas de la prod, configurée par l'env).
	if err := bindEnvs(v, reflect.TypeOf(GlobalConfig{}), ""); err != nil {
		return nil, err
	}

	if err := v.ReadInConfig(); err != nil {
		// En production, tout peut venir de l'environnement : l'image n'embarque
		// aucun fichier et ValidateProduction signale ce qui manque. En
		// développement, un fichier absent reste une erreur.
		if !isProductionEnv(v.GetString(envKey)) || !isConfigFileMissing(err) {
			return nil, fmt.Errorf("erreur de chargement du fichier de configuration: %w", err)
		}
	}

	var config GlobalConfig
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("erreur d'analyse du fichier de configuration: %w", err)
	}

	if config.Server.ShutdownTimeout <= 0 {
		config.Server.ShutdownTimeout = DefaultShutdownTimeout
	}

	if config.IsProduction() {
		config.applyProductionDefaults()
		if err := config.ValidateProduction(); err != nil {
			return nil, err
		}
	}

	AppConfig = config
	return &AppConfig, nil
}

// isConfigFileMissing : fichier introuvable, recherché (nom + répertoires) ou
// désigné par son chemin.
func isConfigFileMissing(err error) bool {
	var notFound viper.ConfigFileNotFoundError
	return errors.As(err, &notFound) || errors.Is(err, fs.ErrNotExist)
}

// bindEnvs lie chaque clé de la configuration (tags mapstructure, récursif) à
// sa variable KUBEMANAGER_<CLÉ>.
func bindEnvs(v *viper.Viper, t reflect.Type, prefix string) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("mapstructure"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}
		if f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeOf(time.Duration(0)) && f.Type.PkgPath() != "time" {
			if err := bindEnvs(v, f.Type, key); err != nil {
				return err
			}
			continue
		}
		if err := v.BindEnv(key); err != nil {
			return fmt.Errorf("config: bind env %s: %w", key, err)
		}
	}
	return nil
}
