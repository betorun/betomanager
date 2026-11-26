package service

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	computeDomain "github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
)

// Lot 2 de l'ADR 0019 : le réseau privé suit la vie du VPS. Inscription au
// premier démarrage (création, réinstallation), commande pour un VPS
// existant, accès coupé à la suspension, rattachement retiré à la
// résiliation ; la passerelle est tenue à jour depuis la base (source de
// vérité), à chaque changement d'un VPS et par une réconciliation régulière.

// ── Script et commande d'inscription ───────────────────────────────────

func (s *Service) scripter() domain.Scripter {
	for _, b := range s.Backends {
		if sc, ok := b.(domain.Scripter); ok && b.Capabilities().NeedsEnrollment {
			return sc
		}
	}
	return nil
}

func (s *Service) apiURL() string { return strings.TrimRight(strings.TrimSpace(s.APIURL), "/") }

// EnrollScript : script d'inscription servi aux VPS (jeton en premier
// argument) ; "" si aucune mise en œuvre n'en a besoin.
func (s *Service) EnrollScript() string {
	sc := s.scripter()
	if sc == nil || s.apiURL() == "" {
		return ""
	}
	return sc.EnrollScript(s.apiURL(), "")
}

// EnrollScriptPath : chemin du script, sous l'URL publique de l'API.
const EnrollScriptPath = "/vpc/enroll.sh"

// command : à lancer en root sur un VPS existant.
func (s *Service) command(token string) string {
	if s.apiURL() == "" {
		return ""
	}
	return "curl -fsS " + s.apiURL() + EnrollScriptPath + " | sudo sh -s -- " + token
}

// ── Installation (création, réinstallation) ─────────────────────────────

// PrepareInstall : appelé juste avant l'installation du système d'un VPS
// (création ou réinstallation, disque effacé), par le mode compute : en
// base seulement, aucun accès aux clusters. Si le VPS doit être sur le
// réseau privé : rattachement prêt (adresse gardée s'il l'était), ancienne
// clé retirée, jeton neuf ; rend le script à lancer au premier démarrage.
// "" : rien à faire. Un réseau indisponible pour ce VPS n'empêche jamais
// l'installation (le choix est alors retiré, le client le refera).
func (s *Service) PrepareInstall(ctx context.Context, vpsID uuid.UUID) (string, error) {
	v, err := s.Machines.VPS(ctx, vpsID)
	if err != nil {
		return "", err
	}
	a, err := s.Store.Attachment(ctx, v.ID)
	if err != nil {
		return "", err
	}
	if a == nil && !v.PrivateNetwork {
		return "", nil
	}
	if a != nil {
		b := s.backendNamed(a.Backend)
		if b == nil || !b.Capabilities().NeedsEnrollment {
			return "", nil // réseau natif : le VPS reste relié
		}
		if err := s.Store.Reset(ctx, v.ID); err != nil {
			return "", err
		}
	}
	res, err := s.attach(ctx, v, "system", domain.InstallEnrollmentTTL, false)
	if errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrRegionMismatch) || errors.Is(err, domain.ErrRangeExhausted) {
		s.logWarn("private network skipped at install", "vps", v.ID, "error", err)
		return "", s.Store.SetWanted(ctx, v.ID, false)
	}
	if err != nil {
		return "", err
	}
	if res.Enrollment == nil {
		return "", nil
	}
	sc := s.scripter()
	if sc == nil || s.apiURL() == "" {
		return "", nil
	}
	return sc.EnrollScript(s.apiURL(), res.Enrollment.Token), nil
}

// BeforeReinstall : réinstallation demandée (API) : la clé du système
// actuel ne vaut plus, l'accès est coupé tout de suite (un VPS réinstallé
// parce que compromis ne garde pas le réseau privé). L'adresse est gardée ;
// le nouveau système s'inscrit au premier démarrage.
func (s *Service) BeforeReinstall(ctx context.Context, vpsID uuid.UUID) error {
	a, err := s.Store.Attachment(ctx, vpsID)
	if err != nil || a == nil {
		return err
	}
	b := s.backendNamed(a.Backend)
	if b == nil || !b.Capabilities().NeedsEnrollment {
		return nil
	}
	v, err := s.Machines.VPS(ctx, vpsID)
	if err != nil {
		return err
	}
	if err := s.Store.Reset(ctx, vpsID); err != nil {
		return err
	}
	a.State, a.PublicKey, a.AttachedAt = domain.StatePending, "", nil
	return s.ensureMember(ctx, b, a, v)
}

// ensureMember : le membre à jour côté réseau (clé retirée, adresse IP…).
func (s *Service) ensureMember(ctx context.Context, b domain.VPCBackend, a *domain.Attachment, v *computeDomain.VPS) error {
	n, err := s.Store.Network(ctx, a.ProjectID)
	if err != nil || n == nil {
		return err
	}
	vpc, m, err := s.member(n, a, v)
	if err != nil {
		return err
	}
	return b.Ensure(ctx, vpc, []domain.Member{m})
}

// CheckOrder : le réseau privé est possible pour un VPS commandé dans ce
// projet, chez cet hébergeur et à cet emplacement (refus avant tout débit).
func (s *Service) CheckOrder(ctx context.Context, project uuid.UUID, provider, location string) error {
	cl, err := s.Clusters.ProjectCluster(ctx, project)
	if err != nil {
		return err
	}
	if s.RegionOf != nil {
		if r := s.RegionOf(&computeDomain.VPS{Location: location}); r != "" && cl.Region != "" && r != cl.Region {
			return domain.ErrRegionMismatch
		}
	}
	b := s.pick(domain.Placement{VPSProvider: provider, ClusterID: cl.ID, ClusterProvider: cl.Provider})
	if b == nil {
		return domain.ErrUnavailable
	}
	if b.Capabilities().NeedsEnrollment && (s.scripter() == nil || s.apiURL() == "") {
		return domain.ErrUnavailable
	}
	return nil
}

// ── Suivi des VPS ──────────────────────────────────────────────────────

// active : VPS dont le lien est permis. Suspendu : accès coupé, rattachement
// gardé (même adresse, même clé), rendu à la reprise.
func active(v *computeDomain.VPS) bool {
	switch v.State {
	case computeDomain.StateRunning, computeDomain.StateStopped, computeDomain.StateCreating, computeDomain.StatePending:
		return true
	}
	return false
}

// gone : VPS résilié ou dont la création a échoué : rattachement retiré.
func gone(v *computeDomain.VPS) bool {
	return v.State == computeDomain.StateDeleted || v.State == computeDomain.StateFailed
}

// OnVPSChanged : un VPS a changé (état, adresse) ; son réseau privé suit.
func (s *Service) OnVPSChanged(ctx context.Context, v *computeDomain.VPS) {
	a, err := s.Store.Attachment(ctx, v.ID)
	if err != nil || a == nil {
		return
	}
	if err := s.Sync(ctx, a.ProjectID); err != nil {
		s.logWarn("private network sync", "project", a.ProjectID, "vps", v.ID, "error", err)
	}
}

// Sync : réseau d'un projet aligné sur la base : VPS actifs présents (clé,
// adresse IP à jour), suspendus coupés, résiliés retirés, inconnus retirés.
func (s *Service) Sync(ctx context.Context, project uuid.UUID) error {
	n, err := s.Store.Network(ctx, project)
	if err != nil || n == nil {
		return err
	}
	vpc, err := toVPC(n)
	if err != nil {
		return err
	}
	atts, err := s.Store.Attachments(ctx, project)
	if err != nil {
		return err
	}
	if len(atts) == 0 {
		// Plus aucun VPS (plage gardée par le projet) : rien ne doit rester
		// côté réseau, même recréé par une synchronisation concurrente.
		return s.teardown(ctx, project, vpc, s.Backends...)
	}
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	members := map[string][]domain.Member{}
	known := map[uuid.UUID]bool{}
	used := map[string]bool{}
	// Réseau natif rattaché en base seulement (création) : relié dès que le
	// réseau le connaît.
	pendingNative := map[string][]uuid.UUID{}
	for i := range atts {
		a := &atts[i]
		v, err := s.Machines.VPS(ctx, a.VPSID)
		if err != nil {
			keep(err)
			continue
		}
		b := s.backendNamed(a.Backend)
		if b == nil {
			continue
		}
		used[a.Backend] = true
		switch {
		case gone(v):
			keep(s.release(ctx, a))
			if v.State == computeDomain.StateDeleted {
				keep(s.Store.SetWanted(ctx, v.ID, false))
			}
		case active(v):
			known[a.VPSID] = true
			_, m, err := s.member(n, a, v)
			if err != nil {
				keep(err)
				continue
			}
			members[a.Backend] = append(members[a.Backend], m)
			if a.State == domain.StatePending && !b.Capabilities().NeedsEnrollment {
				pendingNative[a.Backend] = append(pendingNative[a.Backend], a.VPSID)
			}
		default:
			known[a.VPSID] = true
			keep(b.Detach(ctx, vpc, a.VPSID)) // suspendu : accès coupé
		}
	}
	for name := range used {
		b := s.backendNamed(name)
		if ms := members[name]; len(ms) > 0 {
			err := b.Ensure(ctx, vpc, ms)
			keep(err)
			if err == nil {
				for _, id := range pendingNative[name] {
					keep(s.Store.SetState(ctx, id, domain.StateAttached, "", s.now()))
				}
			}
		}
		// Membres que la base ne connaît plus (retrait interrompu) ; trafic
		// relevé au passage.
		if list, err := b.Status(ctx, vpc); err == nil {
			counters := map[uuid.UUID]domain.Traffic{}
			for _, st := range list {
				if !known[st.VPSID] {
					keep(b.Detach(ctx, vpc, st.VPSID))
					continue
				}
				if st.Traffic != nil {
					counters[st.VPSID] = *st.Traffic
				}
			}
			if err := s.Store.RecordTraffic(ctx, project, s.now(), counters); err != nil {
				s.logWarn("private network traffic", "project", project, "error", err)
			}
		}
	}
	return first
}

// Reconcile : tous les VPC alignés sur la base (filet après une coupure).
func (s *Service) Reconcile(ctx context.Context) error {
	list, err := s.Store.Networks(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, n := range list {
		if err := s.Sync(ctx, n.ProjectID); err != nil {
			s.logWarn("private network reconcile", "project", n.ProjectID, "error", err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// DefaultReconcileEvery : intervalle de la réconciliation (worker).
const DefaultReconcileEvery = 2 * time.Minute

// RunReconcile : Reconcile à intervalle jusqu'à l'arrêt.
func (s *Service) RunReconcile(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.Reconcile(ctx)
		}
	}
}

func (s *Service) logWarn(msg string, args ...any) {
	if s.Logger != nil {
		s.Logger.Warnw(msg, args...)
	}
}

// ── Adresse source de l'inscription ────────────────────────────────────

// publicSource : adresse de la requête, et si elle est publique (une
// adresse privée vient d'un mandataire : rien à comparer).
func publicSource(ip string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return netip.Addr{}, false
	}
	a = a.Unmap()
	return a, a.IsGlobalUnicast() && !a.IsPrivate()
}

func sameAddr(a netip.Addr, s string) bool {
	b, err := netip.ParseAddr(strings.TrimSpace(s))
	return err == nil && a == b.Unmap()
}

// ── Trafic (équipe) ────────────────────────────────────────────────────

// TrafficView : trafic du réseau privé de chaque projet sur un mois.
type TrafficView struct {
	// Month : mois relevé (AAAA-MM, UTC).
	Month    string                  `json:"month"`
	Projects []domain.ProjectTraffic `json:"projects"`
	// Totaux du mois, tous projets.
	FromVPSBytes int64 `json:"from_vps_bytes"`
	ToVPSBytes   int64 `json:"to_vps_bytes"`
	// Every : intervalle du relevé (au plus).
	EverySeconds int `json:"every_seconds"`
}

// Traffic : relevé d'un mois ("" : mois en cours). Mesure seulement : rien
// n'est facturé (prix : décision du propriétaire, ADR 0019).
func (s *Service) Traffic(ctx context.Context, month string) (*TrafficView, error) {
	at := s.now()
	m := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	if month = strings.TrimSpace(month); month != "" {
		t, err := time.Parse("2006-01", month)
		if err != nil {
			return nil, domain.ErrInvalidMonth
		}
		m = t
	}
	list, err := s.Store.Traffic(ctx, m)
	if err != nil {
		return nil, err
	}
	v := &TrafficView{Month: m.Format("2006-01"), Projects: list, EverySeconds: int(DefaultReconcileEvery / time.Second)}
	for _, p := range list {
		v.FromVPSBytes += p.FromVPSBytes
		v.ToVPSBytes += p.ToVPSBytes
	}
	return v, nil
}
