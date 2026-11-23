/*
	 egress-agent : monte sur le nœud passerelle les IP de sortie dédiées des
	 projets (ADR 0014, lot 2). Un pod par nœud (DaemonSet, réseau de l'hôte,
	 capacité NET_ADMIN seulement), posé par « Préparer le cluster ».

	 Configuration (variables d'environnement) :

		NODE_NAME          nom du nœud (API descendante, requis)
		EGRESS_INTERFACE   interface de sortie (défaut : celle de la route par défaut)
		EGRESS_EVERY       intervalle des passages (défaut 15s)
*/
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/thekrauss/kubemanager/internal/egressagent"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	node := os.Getenv("NODE_NAME")
	if node == "" {
		logger.Error("NODE_NAME is required")
		os.Exit(1)
	}
	every := 15 * time.Second
	if v := os.Getenv("EGRESS_EVERY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			logger.Error("invalid EGRESS_EVERY", "value", v)
			os.Exit(1)
		}
		every = d
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("in-cluster config", "error", err)
		os.Exit(1)
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		logger.Error("kubernetes client", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("egress agent started", "node", node)
	(&egressagent.Agent{Kube: kube, Node: node, Interface: os.Getenv("EGRESS_INTERFACE"),
		Run: egressagent.IP, Every: every, Logger: logger}).Loop(ctx)
	// Les adresses restent montées à l'arrêt : un redémarrage de l'agent ne
	// coupe pas le trafic des projets.
}
