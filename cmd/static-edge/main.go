// static-edge : sert les sites statiques depuis l'object store.
//
// Binaire séparé de la plateforme (cmd/gateway) : il est exposé à Internet et
// ne doit embarquer ni identifiants admin, ni accès en écriture. Il lit :
//   - nom d'hôte → site + déploiement actif : en production, par l'API
//     (EDGE_API_URL, clé du compte système du cluster) ; en développement,
//     Postgres en lecture seule (EDGE_DATABASE_URL) ;
//   - l'object store en lecture seule (fichiers des déploiements).
//
// Production : dans le cluster, derrière Traefik (certificats cert-manager).
// Développement : derrière Caddy (TLS à la demande validé par /check).
//
// Configuration (variables d'environnement) :
//
//	EDGE_HTTP_ADDR       port public            (défaut :8090)
//	EDGE_INTERNAL_ADDR   /check + /healthz      (défaut :8091)
//	EDGE_API_URL         …/kmanager/v1 : mode cluster, avec EDGE_KEY_FILE
//	                     (fichier de clé du watcher) ; EDGE_CLUSTER_ID lu
//	                     dans ce fichier par défaut
//	EDGE_DATABASE_URL    DSN Postgres lecture seule (sans EDGE_API_URL)
//	EDGE_BASE_DOMAIN     domaine des sous-domaines de sites (requis)
//	EDGE_CACHE_TTL       cache nom d'hôte → site (défaut 10s)
//	EDGE_S3_ENDPOINT, EDGE_S3_ACCESS_KEY, EDGE_S3_SECRET_KEY, EDGE_S3_BUCKET (requis)
//	EDGE_S3_REGION, EDGE_S3_USE_SSL
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/edge"
	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("static-edge stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	var source edge.Resolver
	if cfg.apiURL != "" {
		tokens, err := api.KeyFileSource(cfg.keyFile)
		if err != nil {
			return fmt.Errorf("%s: %w", envKeyFile, err)
		}
		source = &edge.HTTPResolver{BaseURL: cfg.apiURL, ClusterID: cfg.clusterID, Tokens: tokens,
			Client: &http.Client{Timeout: apiClientTimeout}}
		logger.Info("site lookup through the API", "cluster", cfg.clusterID)
	} else {
		db, err := sql.Open(sqlDriverName, cfg.databaseURL)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer db.Close()
		db.SetMaxOpenConns(dbMaxOpenConns)
		db.SetConnMaxIdleTime(dbConnMaxIdleTime)
		source = &edge.SQLResolver{DB: db, BaseDomain: cfg.baseDomain}
	}
	resolver := edge.NewCachedResolver(source, cfg.cacheTTL, 0)
	resolver.StaleFor = defaultStaleFor

	store, err := objectstore.New(cfg.store)
	if err != nil {
		return err
	}

	srv := &edge.Server{
		Resolver: resolver,
		Store:    store,
		Logger:   logger,
	}
	public := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: publicReadHeaderTimeout,
		IdleTimeout:       publicIdleTimeout,
	}
	internal := &http.Server{
		Addr:              cfg.internalAddr,
		Handler:           srv.InternalHandler(),
		ReadHeaderTimeout: internalReadHeaderTimeout,
	}

	errCh := make(chan error, 2)
	for _, s := range []*http.Server{public, internal} {
		go func(s *http.Server) {
			logger.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(s)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = internal.Shutdown(shutdownCtx)
	return public.Shutdown(shutdownCtx)
}

type config struct {
	httpAddr     string
	internalAddr string
	databaseURL  string
	apiURL       string
	clusterID    string
	keyFile      string
	baseDomain   string
	cacheTTL     time.Duration
	store        objectstore.Config
}

func loadConfig() (config, error) {
	cfg := config{
		httpAddr:     envOr(envHTTPAddr, defaultHTTPAddr),
		internalAddr: envOr(envInternalAddr, defaultInternalAddr),
		databaseURL:  os.Getenv(envDatabaseURL),
		apiURL:       strings.TrimRight(os.Getenv(envAPIURL), "/"),
		clusterID:    os.Getenv(envClusterID),
		keyFile:      os.Getenv(envKeyFile),
		baseDomain:   strings.TrimSuffix(strings.ToLower(os.Getenv(envBaseDomain)), "."),
		cacheTTL:     defaultCacheTTL,
		store: objectstore.Config{
			Endpoint:  os.Getenv(envS3Endpoint),
			AccessKey: os.Getenv(envS3AccessKey),
			SecretKey: os.Getenv(envS3SecretKey),
			Bucket:    os.Getenv(envS3Bucket),
			Region:    os.Getenv(envS3Region),
		},
	}
	if v := os.Getenv(envCacheTTL); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", envCacheTTL, err)
		}
		cfg.cacheTTL = d
	}
	if v := os.Getenv(envS3UseSSL); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", envS3UseSSL, err)
		}
		cfg.store.UseSSL = b
	}

	required := map[string]string{
		envBaseDomain:  cfg.baseDomain,
		envS3Endpoint:  cfg.store.Endpoint,
		envS3AccessKey: cfg.store.AccessKey,
		envS3SecretKey: cfg.store.SecretKey,
		envS3Bucket:    cfg.store.Bucket,
	}
	if cfg.apiURL != "" {
		if cfg.clusterID == "" && cfg.keyFile != "" {
			cfg.clusterID = clusterOfKeyFile(cfg.keyFile)
		}
		required[envClusterID], required[envKeyFile] = cfg.clusterID, cfg.keyFile
	} else {
		required[envDatabaseURL] = cfg.databaseURL
	}
	var missing []string
	for name, v := range required {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return cfg, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// clusterOfKeyFile : cluster_id du fichier de clé (celui du watcher) ; "" si illisible.
func clusterOfKeyFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var f api.KeyFile
	if json.Unmarshal(raw, &f) != nil {
		return ""
	}
	return f.ClusterID
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
