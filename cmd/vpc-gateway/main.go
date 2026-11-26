/*
vpc-gateway : passerelle du réseau privé (VPC) d'un projet (ADR 0019,
mise en œuvre « tunnel »). Un pod par projet, posé par l'API dans l'espace
du projet ; root dans le conteneur mais seulement la capacité NET_ADMIN,
dans l'espace réseau du pod (jamais celui du nœud).

Fichiers montés : configuration (ConfigMap) et clé privée (Secret), voir
internal/vpcgateway. Variable facultative : VPC_SYNC_EVERY (défaut 5s).
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

	"github.com/thekrauss/kubemanager/internal/vpcgateway"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	every := 5 * time.Second
	if v := os.Getenv("VPC_SYNC_EVERY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			logger.Error("invalid VPC_SYNC_EVERY", "value", v)
			os.Exit(1)
		}
		every = d
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a := &vpcgateway.Agent{Run: vpcgateway.Exec, Output: vpcgateway.ExecOutput, Every: every, Logger: logger}
	srv := &http.Server{Addr: fmt.Sprintf(":%d", vpcgateway.StatusPort), Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("status server", "error", err)
			stop()
		}
	}()
	logger.Info("vpc gateway started")
	a.Loop(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}
