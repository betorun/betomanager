package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// ErrNoProvider : le mode compute n'a pas de clés du fournisseur.
var ErrNoProvider = errors.New("compute: no provider configured")

// ErrReconcileRunning : une passe tourne déjà (périodique et manuelle ensemble).
var ErrReconcileRunning = errors.New("compute: a reconciliation is already running")

const (
	// runStaleAfter : une passe non close depuis ce délai est tenue pour morte.
	runStaleAfter = 30 * time.Minute
	// backupsEvery : sauvegardes relevées une fois par heure (réparties sur
	// les passes de 10 minutes) ; le reste à chaque passe.
	backupsEvery = time.Hour
	passEvery    = 10 * time.Minute
	// refreshConcurrency : relevés par VPS en parallèle (débit du client
	// fournisseur plafonné de son côté).
	refreshConcurrency = 4
	// requestsKept, runsKept : historique gardé (demandes closes, passes).
	requestsKept = 90 * 24 * time.Hour
	runsKept     = 30 * 24 * time.Hour
	// saveRunTimeout : clôture de la passe, même si son contexte est annulé.
	saveRunTimeout = 10 * time.Second
)

// Reconciler : rapproche la base du fournisseur. Elle ne corrige que ce qui
// est sûr (marche ↔ arrêt, adresses d'un VPS sans opération en cours) ; le
// reste est relevé pour l'équipe (écarts), jamais réparé en silence.
type Reconciler struct {
	Store    Store
	Provider ComputeProvider
	// Backups nil : ni relevé des sauvegardes ni annulation des snapshots bloqués.
	Backups BackupProvider
	// Network nil : blocages anti-spam non relevés.
	Network NetworkProvider
	// Stock nil : stock non relevé ; Inbox nil : e-mails du fournisseur non relevés.
	Stock  StockProvider
	Inbox  Inbox
	Events Events
	// StuckAfter : âge au-delà duquel un snapshot en cours est annulé (0 : 6 h).
	StuckAfter time.Duration
	// Heartbeat : appelé au fil de la passe (activité Temporal vivante) ; nil : rien.
	Heartbeat func()
	Now       func() time.Time
}

func (r *Reconciler) beat() {
	if r.Heartbeat != nil {
		r.Heartbeat()
	}
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// settled : états que la réconciliation peut corriger d'elle-même.
func settled(state string) bool {
	return state == domain.StateRunning || state == domain.StateStopped
}

// expectedAtProvider : un VPS dans cet état doit exister chez le fournisseur.
func expectedAtProvider(state string) bool {
	switch state {
	case domain.StateRunning, domain.StateStopped, domain.StateSuspended, domain.StateCreating:
		return true
	}
	return false
}

// Run : une passe complète ; elle est enregistrée, réussie ou non. Une seule
// à la fois (ErrReconcileRunning sinon).
func (r *Reconciler) Run(ctx context.Context) (*domain.ReconcileRun, error) {
	if r.Provider == nil {
		return nil, ErrNoProvider
	}
	run := &domain.ReconcileRun{ID: uuid.New(), Provider: r.Provider.Name(), StartedAt: r.now()}
	ok, err := r.Store.BeginRun(ctx, run, runStaleAfter)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrReconcileRunning
	}
	err = r.pass(ctx, run)
	done := r.now()
	run.FinishedAt = &done
	if err != nil {
		run.Error = err.Error()
		if len(run.Error) > 500 {
			run.Error = run.Error[:500]
		}
	}
	// Contexte détaché : une passe annulée (délai, arrêt) est quand même close.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveRunTimeout)
	defer cancel()
	if saveErr := r.Store.SaveRun(saveCtx, run); saveErr != nil && err == nil {
		err = saveErr
	}
	return run, err
}

func (r *Reconciler) pass(ctx context.Context, run *domain.ReconcileRun) error {
	provider := r.Provider.Name()
	vms, err := r.Provider.List(ctx)
	if err != nil {
		return err
	}
	locals, err := r.Store.ProviderVPS(ctx, provider)
	if err != nil {
		return err
	}
	byID := make(map[string]domain.VPS, len(locals))
	for _, v := range locals {
		byID[v.ProviderID] = v
	}

	seenAt := r.now()
	machines := make([]domain.ProviderMachine, 0, len(vms))
	present := make(map[string]bool, len(vms))
	var errs []error
	for _, vm := range vms {
		present[vm.ProviderID] = true
		m := domain.ProviderMachine{
			Provider: provider, ProviderID: vm.ProviderID, State: vm.State, RawState: vm.RawState,
			Plan: vm.Plan, Zone: vm.Zone, IPv4: vm.IPv4, IPv6: vm.IPv6, Busy: vm.Busy, SeenAt: seenAt,
		}
		local, known := byID[vm.ProviderID]
		if known {
			id := local.ID
			m.VPSID = &id
		}
		switch {
		case vm.Unread:
			// Illisible à cette passe : présent, relevé précédent gardé.
			m.Keep = true
			run.Unread++
		case !known:
			m.Drift = domain.DriftOrphan
			run.Orphans++
		case vm.Busy:
			// Opération en cours chez le fournisseur : on relèvera à la passe
			// suivante ; un snapshot bloqué depuis trop longtemps est annulé.
			r.abortStuck(ctx, vm.ProviderID)
		case settled(local.State) && settled(vm.State):
			ipv4, ipv6 := keep(vm.IPv4, local.IPv4), keep(vm.IPv6, local.IPv6)
			if local.State != vm.State || local.IPv4 != ipv4 || local.IPv6 != ipv6 {
				ok, err := r.Store.ApplyObserved(ctx, local.ID, local.Version, vm.State, ipv4, ipv6, seenAt)
				switch {
				case err != nil:
					errs = append(errs, err) // les autres VPS sont quand même relevés
				case ok:
					run.Updated++
				}
			}
		default:
			// En création : le workflow de commande s'en charge. Résilié et
			// arrêté : attend sa suppression à l'expiration chez le fournisseur.
			resigned := local.State == domain.StateDeleted && vm.State == domain.StateStopped
			if local.State != domain.StateCreating && local.State != vm.State && !resigned {
				m.Drift = domain.DriftState
				run.Drifts++
			}
		}
		machines = append(machines, m)
	}
	run.Seen = len(machines)
	r.beat()
	r.refreshBackups(ctx, locals, machines)
	r.refreshSpam(ctx, locals, machines)
	r.refreshStock(ctx)
	r.collectMails(ctx, locals)
	now := r.now()
	_ = r.Store.Purge(ctx, now, now.Add(-requestsKept), now.Add(-runsKept)) // reprise à la passe suivante
	for _, v := range locals {
		if !present[v.ProviderID] && expectedAtProvider(v.State) {
			run.Missing++
		}
	}
	if err := r.Store.ReplaceMachines(ctx, provider, machines); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d error(s), first: %w", len(errs), errs[0])
	}
	return nil
}

// eachVPS : fn sur chaque VPS retenu, refreshConcurrency à la fois.
func (r *Reconciler) eachVPS(ctx context.Context, vs []*domain.VPS, fn func(v *domain.VPS)) {
	sem := make(chan struct{}, refreshConcurrency)
	var wg sync.WaitGroup
	for i, v := range vs {
		if ctx.Err() != nil {
			break
		}
		if i%20 == 0 {
			r.beat()
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(v *domain.VPS) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(v)
		}(v)
	}
	wg.Wait()
}

// readable : VPS relevé à cette passe, sans opération en cours.
func readable(machines []domain.ProviderMachine) map[string]bool {
	out := make(map[string]bool, len(machines))
	for _, m := range machines {
		out[m.ProviderID] = !m.Busy && !m.Keep
	}
	return out
}

// dueThisPass : relevé « toutes les every », réparti par VPS sur les passes
// (une passe toutes les passEvery) pour lisser les appels au fournisseur.
func dueThisPass(id uuid.UUID, now time.Time, every time.Duration) bool {
	slots := int64(every / passEvery)
	if slots <= 1 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write(id[:])
	return int64(h.Sum32())%slots == (now.Unix()/int64(passEvery.Seconds()))%slots
}

// keep : une adresse absente du relevé ne remplace pas celle connue.
func keep(observed, known string) string {
	if observed == "" {
		return known
	}
	return observed
}

func (r *Reconciler) abortStuck(ctx context.Context, providerID string) {
	if r.Backups == nil {
		return
	}
	after := r.StuckAfter
	if after <= 0 {
		after = 6 * time.Hour
	}
	_, _ = r.Backups.AbortStuck(ctx, providerID, after) // passe suivante si l'appel échoue
}

// refreshBackups : relevé horaire des sauvegardes des VPS livrés, sans
// opération en cours ; un échec n'arrête pas la passe (relevé précédent gardé).
func (r *Reconciler) refreshBackups(ctx context.Context, locals []domain.VPS, machines []domain.ProviderMachine) {
	if r.Backups == nil {
		return
	}
	ok, now := readable(machines), r.now()
	var due []*domain.VPS
	for i := range locals {
		v := &locals[i]
		if !ok[v.ProviderID] || v.Operation != "" || !expectedAtProvider(v.State) || v.State == domain.StateCreating || !dueThisPass(v.ID, now, backupsEvery) {
			continue
		}
		due = append(due, v)
	}
	r.eachVPS(ctx, due, func(v *domain.VPS) {
		pb, err := r.Backups.Backups(ctx, v.ProviderID)
		if err != nil {
			return
		}
		if changed, err := SaveBackups(ctx, r.Store, v.ID, pb, r.now()); err == nil && changed && r.Events != nil {
			r.Events.BackupsChanged(ctx, v)
		}
	})
}

// refreshSpam : blocage anti-spam des adresses IPv4 (relevé ; le déblocage
// est une décision de l'équipe).
func (r *Reconciler) refreshSpam(ctx context.Context, locals []domain.VPS, machines []domain.ProviderMachine) {
	if r.Network == nil {
		return
	}
	read := map[string]bool{} // adresse indépendante d'une opération en cours
	for _, m := range machines {
		read[m.ProviderID] = !m.Keep
	}
	var due []*domain.VPS
	for i := range locals {
		v := &locals[i]
		if !read[v.ProviderID] || v.IPv4 == "" || v.State == domain.StateDeleted || v.State == domain.StateCreating {
			continue
		}
		due = append(due, v)
	}
	r.eachVPS(ctx, due, func(v *domain.VPS) {
		blocked, since, err := r.Network.SpamBlocked(ctx, v.IPv4)
		if err != nil {
			return
		}
		switch {
		case blocked && v.SpamBlockedAt == nil:
			if since.IsZero() {
				since = r.now()
			}
			_ = r.Store.UpdateVPS(ctx, v.ID, map[string]any{"spam_blocked_at": since, "updated_at": r.now()})
			Notify(ctx, r.Store, r.Events, v.ID)
		case !blocked && v.SpamBlockedAt != nil:
			_ = r.Store.UpdateVPS(ctx, v.ID, map[string]any{"spam_blocked_at": nil, "updated_at": r.now()})
			Notify(ctx, r.Store, r.Events, v.ID)
		}
	})
}

// refreshStock : stock de chaque offre rattachée à un plan du fournisseur.
func (r *Reconciler) refreshStock(ctx context.Context) {
	if r.Stock == nil {
		return
	}
	offers, err := r.Store.Offers(ctx)
	if err != nil {
		return
	}
	at := r.now()
	for _, o := range offers {
		if o.ProviderPlanCode == "" {
			continue
		}
		rows, err := r.Stock.Stock(ctx, o.ProviderPlanCode)
		if err != nil {
			continue // relevé précédent gardé
		}
		stock := make([]domain.Stock, 0, len(rows))
		for _, row := range rows {
			stock = append(stock, domain.Stock{Offer: o.Code, Location: row.Location, Status: row.Status, Days: row.Days, CheckedAt: at})
		}
		_ = r.Store.ReplaceStock(ctx, o.Code, stock)
	}
}

// mailWindow : e-mails relevés depuis ce délai (un e-mail relevé est effacé de la boîte).
const mailWindow = 7 * 24 * time.Hour

var (
	abuseWords    = []string{"abuse", "abus", "spam", "phishing", "hameçonnage", "malware", "botnet", "ddos", "attaque", "attack", "scan", "plainte", "complaint"}
	incidentWords = []string{"incident", "panne", "outage", "maintenance", "travaux", "intervention", "dégradation", "degradation", "interruption"}
)

// ClassifyMail : abus, incident ou autre (mots du sujet puis du texte).
func ClassifyMail(subject, text string) string {
	for _, part := range []string{strings.ToLower(subject), strings.ToLower(text)} {
		for _, w := range abuseWords {
			if strings.Contains(part, w) {
				return domain.MailAbuse
			}
		}
		for _, w := range incidentWords {
			if strings.Contains(part, w) {
				return domain.MailIncident
			}
		}
	}
	return domain.MailOther
}

// collectMails : e-mails du fournisseur rangés pour l'équipe (rattachés au
// VPS qu'ils citent), puis effacés de la boîte.
func (r *Reconciler) collectMails(ctx context.Context, locals []domain.VPS) {
	if r.Inbox == nil {
		return
	}
	mails, err := r.Inbox.Recent(ctx, r.now().Add(-mailWindow))
	if err != nil {
		return
	}
	var done []uint32
	for _, m := range mails {
		body := m.Text
		if len(body) > 20000 {
			body = body[:20000]
		}
		pm := &domain.ProviderMail{ReceivedAt: m.Date, Sender: m.From, Subject: m.Subject, Body: body,
			Kind: ClassifyMail(m.Subject, m.Text), Fingerprint: mailFingerprint(m), CreatedAt: r.now()}
		if pm.ReceivedAt.IsZero() {
			pm.ReceivedAt = r.now()
		}
		all := m.Subject + "\n" + m.Text
		for i := range locals {
			v := &locals[i]
			if (v.ProviderID != "" && containsToken(all, v.ProviderID)) || (v.IPv4 != "" && containsToken(all, v.IPv4)) {
				id := v.ID
				pm.VPSID = &id
				break
			}
		}
		if err := r.Store.SaveProviderMail(ctx, pm); err != nil {
			continue // gardé dans la boîte : repris à la passe suivante
		}
		done = append(done, m.ID)
	}
	_ = r.Inbox.Delete(ctx, done...) // en échec : rangés à nouveau sans doublon (empreinte)
}

// mailFingerprint : même e-mail relu ⇒ même empreinte.
func mailFingerprint(m InboxMail) string {
	h := sha256.New()
	for _, part := range []string{m.From, m.Subject, m.Date.UTC().Format(time.RFC3339), m.Text} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// containsToken : needle apparaît comme un mot entier (ni chiffre, lettre,
// point ni tiret collé) : « 1.2.3.4 » ne trouve pas « 11.2.3.45 ».
func containsToken(haystack, needle string) bool {
	for i := 0; ; {
		j := strings.Index(haystack[i:], needle)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(needle)
		if (start == 0 || !tokenByte(haystack[start-1])) && (end == len(haystack) || !tokenByte(haystack[end])) {
			return true
		}
		i = start + 1
	}
}

func tokenByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '.' || b == '-' || b == '_'
}
