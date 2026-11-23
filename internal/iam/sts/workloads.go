package sts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/iam"
)

// Identité des workloads (iam.md §7.2) : un pod présente le jeton Kubernetes
// projeté de son compte « system:serviceaccount:<namespace>:<workload> »
// (audience = émetteur du STS). Il est vérifié avec les clés que le watcher
// du cluster a rapportées ; le workload correspondant désigne le compte de
// service betorun. Aucune clé à gérer, aucune relation de confiance à créer.

const (
	// MethodWorkload : jeton obtenu par un pod d'un workload betorun.
	MethodWorkload    = "workload"
	workloadRefPrefix = "workload:"
	k8sSubjectPrefix  = "system:serviceaccount:"
)

// clusterKeys : clés d'un cluster, reconstruites quand le watcher en publie de nouvelles.
type clusterKeys struct {
	updatedAt time.Time
	jwks      *keyfunc.JWKS
}

type workloadKeys struct {
	mu    sync.Mutex
	cache map[string]clusterKeys
}

func (w *workloadKeys) keyfunc(clusterID string, updatedAt time.Time, raw []byte) (jwt.Keyfunc, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c, ok := w.cache[clusterID]; ok && c.updatedAt.Equal(updatedAt) {
		return c.jwks.Keyfunc, nil
	}
	jwks, err := keyfunc.NewJSON(json.RawMessage(raw))
	if err != nil {
		return nil, err
	}
	if w.cache == nil {
		w.cache = map[string]clusterKeys{}
	}
	w.cache[clusterID] = clusterKeys{updatedAt: updatedAt, jwks: jwks}
	return jwks.Keyfunc, nil
}

// exchangeWorkload : handled=false si le jeton ne vient d'aucun cluster
// connu (l'échange continue alors avec les relations de confiance).
//
// Multi-cluster (lot 2c) : plusieurs clusters peuvent publier le même
// émetteur (celui de k3s est identique partout) ; c'est la clé qui a signé
// le jeton qui désigne le cluster. Le workload doit appartenir à un projet
// placé sur ce cluster : un pod d'un cluster ne peut pas se faire passer
// pour un workload d'un autre, même namespace et même nom.
func (s *Service) exchangeWorkload(ctx context.Context, subjectToken, rawIssuer string) (tok *Token, p iam.Principal, handled bool, err error) {
	if s.WorkloadClusterID == "" {
		return nil, "", false, nil
	}
	var keys []struct {
		ClusterID string
		JWKS      []byte
		UpdatedAt time.Time
	}
	if err := s.DB.WithContext(ctx).Raw(`SELECT cluster_id, jwks, updated_at FROM cluster_oidc_keys WHERE issuer = ? ORDER BY cluster_id`,
		rawIssuer).Scan(&keys).Error; err != nil {
		return nil, "", true, err
	}
	if len(keys) == 0 {
		return nil, "", false, nil
	}
	var (
		c       jwt.RegisteredClaims
		cluster string
	)
	for _, k := range keys {
		kf, kerr := s.workloadKeys.keyfunc(k.ClusterID, k.UpdatedAt, k.JWKS)
		if kerr != nil {
			err = kerr
			continue
		}
		c = jwt.RegisteredClaims{}
		_, err = jwt.ParseWithClaims(subjectToken, &c, kf, jwt.WithValidMethods([]string{"RS256", "ES256"}),
			jwt.WithIssuer(rawIssuer), jwt.WithAudience(s.Issuer), jwt.WithExpirationRequired(),
			jwt.WithLeeway(leeway), jwt.WithTimeFunc(s.now))
		// Clé inconnue ou signature fausse : peut-être un autre cluster du
		// même émetteur. Toute autre erreur : signé par ce cluster, refusé.
		if err != nil && (errors.Is(err, jwt.ErrTokenSignatureInvalid) || errors.Is(err, jwt.ErrTokenUnverifiable)) {
			continue
		}
		cluster = k.ClusterID
		break
	}
	if err != nil {
		if errors.Is(err, jwt.ErrTokenInvalidAudience) {
			return nil, "", true, invalidGrant(fmt.Sprintf("subject_token audience must be %q", s.Issuer))
		}
		return nil, "", true, invalidGrant("subject_token: " + describe(err))
	}
	if cluster == "" {
		return nil, "", true, invalidGrant("subject_token: no cluster key verifies it")
	}
	ns, name, ok := strings.Cut(strings.TrimPrefix(c.Subject, k8sSubjectPrefix), ":")
	if !strings.HasPrefix(c.Subject, k8sSubjectPrefix) || !ok || ns == "" || name == "" {
		return nil, "", true, invalidGrant("subject_token is not a Kubernetes service account token")
	}

	var workloadID, saID uuid.UUID
	// Projet sans cluster (avant le registre) : celui de la configuration.
	err = s.DB.WithContext(ctx).Raw(`SELECT w.id, w.service_account_id FROM workloads w
		JOIN service_accounts sa ON sa.id = w.service_account_id
		LEFT JOIN projects p ON p.id = w.project_id
		WHERE w.namespace = ? AND w.name = ? AND sa.deleted_at IS NULL AND NOT sa.disabled
		  AND COALESCE(p.cluster_id, ?) = ?
		ORDER BY w.created_at DESC LIMIT 1`, ns, name, s.WorkloadClusterID, cluster).
		Row().Scan(&workloadID, &saID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", true, invalidGrant(fmt.Sprintf("no betorun workload runs as a service account for %q", c.Subject))
	}
	if err != nil {
		return nil, "", true, err
	}
	principal := iam.ServiceAccountPrincipal(saID)
	token, exp, err := s.Mint(principal, MethodWorkload, workloadRefPrefix+workloadID.String(), FederatedTokenTTL)
	if err != nil {
		return nil, principal, true, err
	}
	audit.Annotate(ctx, "workload", workloadID.String())
	audit.Annotate(ctx, "external_subject", c.Subject)
	audit.Annotate(ctx, "cluster", cluster)
	audit.Annotate(ctx, "token_expires_at", exp)
	return &Token{AccessToken: token, TokenType: "Bearer", ExpiresIn: int(exp.Sub(s.now()).Seconds()),
		IssuedTokenType: TokenTypeAccess}, principal, true, nil
}
