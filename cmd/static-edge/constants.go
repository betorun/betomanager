package main

import "time"

// Variables d'environnement de configuration de l'edge.
const (
	envHTTPAddr     = "EDGE_HTTP_ADDR"
	envInternalAddr = "EDGE_INTERNAL_ADDR"
	envDatabaseURL  = "EDGE_DATABASE_URL"
	envBaseDomain   = "EDGE_BASE_DOMAIN"
	envCacheTTL     = "EDGE_CACHE_TTL"
	envS3Endpoint   = "EDGE_S3_ENDPOINT"
	envS3AccessKey  = "EDGE_S3_ACCESS_KEY"
	envS3SecretKey  = "EDGE_S3_SECRET_KEY"
	envS3Bucket     = "EDGE_S3_BUCKET"
	envS3Region     = "EDGE_S3_REGION"
	envS3UseSSL     = "EDGE_S3_USE_SSL"
	// Mode cluster : la table des sites est lue par l'API, pas dans Postgres.
	envAPIURL    = "EDGE_API_URL"
	envClusterID = "EDGE_CLUSTER_ID"
	envKeyFile   = "EDGE_KEY_FILE"
)

// Valeurs par défaut (candidates à la configuration).
const (
	defaultHTTPAddr     = ":8090"
	defaultInternalAddr = ":8091"
	defaultCacheTTL     = 10 * time.Second
	// defaultStaleFor : API injoignable, les sites déjà connus restent servis.
	defaultStaleFor = 15 * time.Minute
)

// Base de données.
const (
	sqlDriverName     = "pgx"
	dbMaxOpenConns    = 10
	dbConnMaxIdleTime = 5 * time.Minute
	// apiClientTimeout : appel à l'API (mode cluster), échange de jeton compris.
	apiClientTimeout = 10 * time.Second
)

// Serveurs HTTP.
const (
	publicReadHeaderTimeout   = 10 * time.Second
	publicIdleTimeout         = 120 * time.Second
	internalReadHeaderTimeout = 5 * time.Second
	shutdownTimeout           = 15 * time.Second
)
