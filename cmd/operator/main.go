/*
operator : l'Opérateur du back-office (ADR 0016). Tourne sur le serveur
de gestion, hors des serveurs et des clusters qu'il dépanne ; joint par
tunnel SSH. Ne dépend pas de l'API pour démarrer.

Configuration : variables OPERATOR_* (voir internal/operator/config.go).
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thekrauss/kubemanager/internal/agentkit"
	"github.com/thekrauss/kubemanager/internal/core/opjournal"
	"github.com/thekrauss/kubemanager/internal/operator"
	"github.com/thekrauss/kubemanager/internal/operator/kube"
	"github.com/thekrauss/kubemanager/internal/operator/prom"
	"github.com/thekrauss/kubemanager/internal/operator/rescue"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) > 1 && os.Args[1] == "journal-key" {
		if err := journalKeyCLI(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "erreur :", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "rescue" {
		if err := rescueCLI(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "erreur :", err)
			os.Exit(1)
		}
		return
	}
	if err := run(logger); err != nil {
		logger.Error("operator stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := operator.LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	model, err := cfg.LLM()
	if err != nil {
		return err
	}
	var rescueAuth operator.RescueAuth
	if cfg.RescueKey != "" {
		r, err := rescue.New(db, cfg.RescueKey)
		if err != nil {
			return err
		}
		r.Alert = &operator.WebhookAlerter{URL: cfg.AlertWebhook, Logger: logger}
		rescueAuth = r
	} else {
		logger.Warn("OPERATOR_RESCUE_KEY_FILE not set: rescue access disabled")
	}

	clusters := kube.New(nil)
	if cfg.KubeconfigDir != "" {
		if clusters, err = kube.LoadDir(cfg.KubeconfigDir); err != nil {
			return err
		}
	}
	metrics, err := prom.Parse(cfg.Prometheus)
	if err != nil {
		return err
	}
	infrastructure, err := operator.Combine(clusters, metrics)
	if err != nil {
		return err
	}
	var investigator *operator.Investigator
	if model != nil {
		investigator = &operator.Investigator{Store: db, Agent: &agentkit.Agent{LLM: model, Limits: operator.Limits}, Logger: logger}
	}
	if cfg.APIURL == "" {
		logger.Warn("OPERATOR_API_URL not set: only the rescue access will open the Operator")
	}

	// Arrêtée avant la fermeture de la base (defer en ordre inverse).
	purgeCtx, stopPurge := context.WithCancel(ctx)
	defer stopPurge()
	go purge(purgeCtx, db, cfg.ResultRetention, logger)

	switch {
	case cfg.JournalKey == "":
		logger.Warn("OPERATOR_JOURNAL_KEY_FILE not set: the journal is not copied to the platform audit")
	case cfg.APIURL == "":
		logger.Warn("OPERATOR_API_URL not set: the journal is not copied to the platform audit")
	default:
		key, _ := opjournal.ParsePrivateKey(cfg.JournalKey) // vérifiée par LoadConfig
		sync := &operator.JournalSync{Source: db, BaseURL: cfg.APIURL, Key: key, Logger: logger}
		go sync.Run(purgeCtx, time.Minute)
	}

	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: (&operator.Server{Store: db, Rescue: rescueAuth, Auth: &operator.APIAuth{BaseURL: cfg.APIURL},
			Investigator: investigator, Infrastructure: infrastructure, API: &operator.APITools{BaseURL: cfg.APIURL}, Model: cfg.Model, Origins: cfg.ConsoleOrigins, Logger: logger}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("operator started", "listen", cfg.Listen, "agent", model != nil, "model", cfg.Model, "clusters", clusters.Names(), "prometheus", metrics.Names())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// purge : efface chaque heure les résultats complets au-delà de la durée de
// conservation (le condensé reste).
func purge(ctx context.Context, db *store.Store, keep time.Duration, logger *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := db.PurgeResults(ctx, time.Now().Add(-keep))
		if err != nil && ctx.Err() == nil {
			logger.Warn("operator purge failed", "error", err)
		} else if n > 0 {
			logger.Info("operator results purged", "steps", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
