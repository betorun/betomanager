// Package service : état des composants (sondes), incidents et page de
// statut publique (docs/modules/status.md).
package service

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/status/domain"
)

// Probe : état d'un composant, lu en base seulement (jamais d'appel externe
// depuis l'API). Une erreur rend l'état unknown, sans détail public.
type Probe interface {
	Check(ctx context.Context, now time.Time) (domain.Check, error)
}

// ProbeFunc : sonde écrite comme une fonction.
type ProbeFunc func(ctx context.Context, now time.Time) (domain.Check, error)

func (f ProbeFunc) Check(ctx context.Context, now time.Time) (domain.Check, error) {
	return f(ctx, now)
}

// Component : composant affiché ; Probe nil : état unknown.
type Component struct {
	Key   string
	Name  string // nom français, traduit par le catalogue
	Probe Probe
}

// Operational : sonde de l'API : si la requête est servie, l'API répond.
var Operational = ProbeFunc(func(context.Context, time.Time) (domain.Check, error) {
	return domain.Check{State: domain.StateOperational}, nil
})

// Store : incidents et leurs messages.
type Store interface {
	CreateIncident(ctx context.Context, i *domain.Incident) error
	// Incident : avec ses messages, du plus ancien au plus récent ; domain.ErrIncidentNotFound.
	Incident(ctx context.Context, id uuid.UUID) (*domain.Incident, error)
	// Incidents : les plus récents d'abord ; open : non résolus seulement.
	Incidents(ctx context.Context, open bool, limit int) ([]domain.Incident, error)
	// Public : non résolus, et résolus depuis since, avec leurs messages.
	Public(ctx context.Context, since time.Time) ([]domain.Incident, error)
	SaveIncident(ctx context.Context, i *domain.Incident) error
	AddUpdate(ctx context.Context, i *domain.Incident, u *domain.IncidentUpdate) error
}

// CacheTTL : la page publique est recalculée au plus toutes les 30 s.
const CacheTTL = 30 * time.Second

// computeTimeout : calcul de la page, détaché de la requête (un visiteur
// qui part n'annule pas le calcul des autres) ; retryAfterError : un calcul
// en échec n'est pas relancé à chaque visite (dernière page gardée).
const (
	computeTimeout  = 5 * time.Second
	retryAfterError = 5 * time.Second
	// staleMax : âge maximal d'une page gardée faute de mieux.
	staleMax = 5 * time.Minute
)

// ResolvedWindow : incidents résolus encore affichés.
const ResolvedWindow = 7 * 24 * time.Hour

// Service : sondes, incidents, page publique.
type Service struct {
	Components []Component
	Store      Store
	Now        func() time.Time

	// mu : un seul calcul à la fois, les visites simultanées l'attendent.
	mu       sync.Mutex
	cached   *snapshot
	cachedAt time.Time
	failedAt time.Time
	lastErr  error
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Known : clé de composant connue.
func (s *Service) Known(key string) bool {
	for _, c := range s.Components {
		if c.Key == key {
			return true
		}
	}
	return false
}

// snapshot : états calculés et incidents, avant traduction.
type snapshot struct {
	at        time.Time
	checks    map[string]domain.Check // sondes seules
	states    map[string]string       // sondes aggravées par les incidents actifs
	incidents []domain.Incident
}

func (s *Service) compute(ctx context.Context) (*snapshot, error) {
	now := s.now()
	snap := &snapshot{at: now, checks: map[string]domain.Check{}, states: map[string]string{}}
	for _, c := range s.Components {
		chk := domain.Check{State: domain.StateUnknown, Reason: domain.ReasonDisabled}
		if c.Probe != nil {
			var err error
			if chk, err = c.Probe.Check(ctx, now); err != nil {
				chk = domain.Check{State: domain.StateUnknown, Reason: domain.ReasonProbeFailed}
			}
		}
		snap.checks[c.Key] = chk
		snap.states[c.Key] = chk.State
	}
	incidents, err := s.Store.Public(ctx, now.Add(-ResolvedWindow))
	if err != nil {
		return nil, err
	}
	snap.incidents = incidents
	for i := range incidents {
		in := &incidents[i]
		if !in.Active(now) {
			continue
		}
		for _, key := range in.Components {
			if st, ok := snap.states[key]; ok {
				snap.states[key] = domain.Worse(st, domain.ImpactState(in.Impact))
			}
		}
	}
	return snap, nil
}

// current : instantané de moins de CacheTTL ; calcul en échec : la dernière
// page est gardée et le calcul n'est retenté qu'après retryAfterError.
func (s *Service) current(ctx context.Context) (*snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.cached != nil && now.Sub(s.cachedAt) < CacheTTL && !now.Before(s.cachedAt) {
		return s.cached, nil
	}
	if s.lastErr != nil && now.Sub(s.failedAt) < retryAfterError && !now.Before(s.failedAt) {
		if s.usableStale(now) {
			return s.cached, nil
		}
		return nil, s.lastErr
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), computeTimeout)
	defer cancel()
	snap, err := s.compute(cctx)
	if err != nil {
		s.failedAt, s.lastErr = now, err
		if s.usableStale(now) {
			return s.cached, nil // page un peu ancienne plutôt qu'une erreur
		}
		return nil, err
	}
	s.cached, s.cachedAt, s.lastErr = snap, now, nil
	return snap, nil
}

func (s *Service) usableStale(now time.Time) bool {
	return s.cached != nil && now.Sub(s.cachedAt) < staleMax
}

// invalidate : un incident a changé, la page publique le montre tout de suite
// (sur ce process ; les autres suivent en 30 s au plus).
func (s *Service) invalidate() {
	s.mu.Lock()
	s.cached, s.lastErr = nil, nil
	s.mu.Unlock()
}

// ── Page publique ───────────────────────────────────────────────────────

// PublicComponent : un composant, son nom traduit, son état.
type PublicComponent struct {
	Key   string `json:"key" description:"Composant : api, vps, domains"`
	Name  string `json:"name" description:"Nom du composant, dans la langue de la requête"`
	State string `json:"state" description:"État : operational, degraded, outage ou unknown"`
}

// PublicUpdate : message daté d'un incident.
type PublicUpdate struct {
	Status    string    `json:"status" description:"Statut de l'incident à ce message : investigating, identified, monitoring ou resolved"`
	Message   string    `json:"message" description:"Message, dans la langue de la requête"`
	CreatedAt time.Time `json:"created_at" description:"Date du message"`
}

// PublicIncident : incident ou maintenance, sans rien d'interne.
type PublicIncident struct {
	ID         uuid.UUID      `json:"id" description:"Identifiant de l'incident"`
	Title      string         `json:"title" description:"Titre, dans la langue de la requête"`
	Message    string         `json:"message" description:"Description, dans la langue de la requête"`
	Impact     string         `json:"impact" description:"Impact : minor, major ou maintenance"`
	Status     string         `json:"status" description:"Statut : investigating, identified, monitoring ou resolved"`
	Components []string       `json:"components" description:"Composants touchés"`
	StartedAt  time.Time      `json:"started_at" description:"Début de l'incident"`
	ResolvedAt *time.Time     `json:"resolved_at,omitempty" description:"Fin de l'incident"`
	StartsAt   *time.Time     `json:"starts_at,omitempty" description:"Début prévu de la maintenance"`
	EndsAt     *time.Time     `json:"ends_at,omitempty" description:"Fin prévue de la maintenance"`
	Updates    []PublicUpdate `json:"updates" description:"Messages, du plus récent au plus ancien"`
}

// PublicStatus : page de statut.
type PublicStatus struct {
	Overall      string            `json:"overall" description:"État d'ensemble : operational, degraded, outage ou unknown"`
	Components   []PublicComponent `json:"components" description:"Composants et leur état"`
	Incidents    []PublicIncident  `json:"incidents" description:"Incidents en cours et résolus depuis 7 jours, les plus récents d'abord"`
	Maintenances []PublicIncident  `json:"maintenances" description:"Maintenances programmées à venir, les plus proches d'abord"`
	UpdatedAt    time.Time         `json:"updated_at" description:"Date du calcul de cette page"`
}

func pick(lang i18n.Locale, fr, en string) string {
	if lang == i18n.EN && en != "" {
		return en
	}
	return fr
}

// Public : page de statut dans une langue (calculée au plus toutes les 30 s).
func (s *Service) Public(ctx context.Context, lang i18n.Locale) (*PublicStatus, error) {
	snap, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	out := &PublicStatus{Components: []PublicComponent{}, Incidents: []PublicIncident{}, Maintenances: []PublicIncident{},
		UpdatedAt: snap.at}
	states := make([]string, 0, len(s.Components))
	for _, c := range s.Components {
		st := snap.states[c.Key]
		states = append(states, st)
		out.Components = append(out.Components, PublicComponent{Key: c.Key, Name: i18n.Translate(lang, c.Name), State: st})
	}
	out.Overall = domain.Overall(states)
	for i := range snap.incidents {
		in := &snap.incidents[i]
		v := publicIncident(in, lang)
		if in.Upcoming(snap.at) {
			out.Maintenances = append([]PublicIncident{v}, out.Maintenances...) // le plus proche d'abord
			continue
		}
		out.Incidents = append(out.Incidents, v)
	}
	return out, nil
}

func publicIncident(in *domain.Incident, lang i18n.Locale) PublicIncident {
	v := PublicIncident{ID: in.ID, Title: pick(lang, in.TitleFR, in.TitleEN), Message: pick(lang, in.MessageFR, in.MessageEN),
		Impact: in.Impact, Status: in.Status, Components: append([]string{}, in.Components...), StartedAt: in.StartedAt,
		ResolvedAt: in.ResolvedAt, StartsAt: in.StartsAt, EndsAt: in.EndsAt, Updates: []PublicUpdate{}}
	for j := len(in.Updates) - 1; j >= 0; j-- {
		u := in.Updates[j]
		v.Updates = append(v.Updates, PublicUpdate{Status: u.Status, Message: pick(lang, u.MessageFR, u.MessageEN), CreatedAt: u.CreatedAt})
	}
	return v
}

// ── Équipe ──────────────────────────────────────────────────────────────

// AdminComponent : état d'un composant, avec la raison de la sonde.
type AdminComponent struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	State      string `json:"state"`
	ProbeState string `json:"probe_state"`
	Reason     string `json:"reason,omitempty"`
}

// AdminStatus : ce que voit la page publique, et pourquoi.
type AdminStatus struct {
	Overall    string           `json:"overall"`
	Components []AdminComponent `json:"components"`
	ComputedAt time.Time        `json:"computed_at"`
}

// Admin : états recalculés (sans cache), avec la raison de chaque sonde.
func (s *Service) Admin(ctx context.Context) (*AdminStatus, error) {
	snap, err := s.compute(ctx)
	if err != nil {
		return nil, err
	}
	out := &AdminStatus{Components: []AdminComponent{}, ComputedAt: snap.at}
	states := []string{}
	for _, c := range s.Components {
		chk := snap.checks[c.Key]
		out.Components = append(out.Components, AdminComponent{Key: c.Key, Name: c.Name, State: snap.states[c.Key],
			ProbeState: chk.State, Reason: chk.Reason})
		states = append(states, snap.states[c.Key])
	}
	out.Overall = domain.Overall(states)
	return out, nil
}

// List : incidents pour l'équipe ; open : non résolus seulement.
func (s *Service) List(ctx context.Context, open bool, limit int) ([]domain.Incident, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.Store.Incidents(ctx, open, limit)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*domain.Incident, error) {
	return s.Store.Incident(ctx, id)
}

// Create : nouvel incident ; son message devient le premier du fil.
func (s *Service) Create(ctx context.Context, in domain.IncidentInput, by string) (*domain.Incident, error) {
	in = in.Normalize()
	if in.Status == "" {
		in.Status = domain.StatusInvestigating
		if in.Impact == domain.ImpactMaintenance {
			in.Status = domain.StatusIdentified
		}
	}
	if err := in.Validate(s.Known); err != nil {
		return nil, err
	}
	now := s.now()
	started := now
	switch {
	case in.StartedAt != nil:
		started = in.StartedAt.UTC()
	case in.StartsAt != nil:
		started = in.StartsAt.UTC()
	}
	i := &domain.Incident{ID: uuid.New(), TitleFR: in.TitleFR, TitleEN: in.TitleEN, MessageFR: in.MessageFR, MessageEN: in.MessageEN,
		Components: in.Components, Impact: in.Impact, Status: in.Status, StartedAt: started, StartsAt: utc(in.StartsAt), EndsAt: utc(in.EndsAt),
		CreatedBy: by, CreatedAt: now, UpdatedBy: by, UpdatedAt: now}
	if i.Resolved() {
		i.ResolvedAt = &now
	}
	i.Updates = []domain.IncidentUpdate{{ID: uuid.New(), IncidentID: i.ID, Status: i.Status, MessageFR: in.MessageFR,
		MessageEN: in.MessageEN, CreatedBy: by, CreatedAt: now}}
	if err := s.Store.CreateIncident(ctx, i); err != nil {
		return nil, err
	}
	s.invalidate()
	return i, nil
}

// Edit : corrige le texte, les composants, l'impact ou la fenêtre (le fil des
// messages ne change pas ; le statut passe par AddUpdate).
func (s *Service) Edit(ctx context.Context, id uuid.UUID, in domain.IncidentInput, by string) (*domain.Incident, error) {
	i, err := s.Store.Incident(ctx, id)
	if err != nil {
		return nil, err
	}
	in = in.Normalize()
	in.Status = ""
	if err := in.Validate(s.Known); err != nil {
		return nil, err
	}
	i.TitleFR, i.TitleEN, i.MessageFR, i.MessageEN = in.TitleFR, in.TitleEN, in.MessageFR, in.MessageEN
	i.Components, i.Impact, i.StartsAt, i.EndsAt = in.Components, in.Impact, utc(in.StartsAt), utc(in.EndsAt)
	if in.StartedAt != nil {
		i.StartedAt = in.StartedAt.UTC()
	}
	i.UpdatedBy, i.UpdatedAt = by, s.now()
	if err := s.Store.SaveIncident(ctx, i); err != nil {
		return nil, err
	}
	s.invalidate()
	return s.Store.Incident(ctx, id)
}

// AddUpdate : nouveau message et nouveau statut ; resolved clôt l'incident.
func (s *Service) AddUpdate(ctx context.Context, id uuid.UUID, in domain.UpdateInput, by string) (*domain.Incident, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	i, err := s.Store.Incident(ctx, id)
	if err != nil {
		return nil, err
	}
	if i.Resolved() {
		return nil, domain.ErrIncidentResolved
	}
	now := s.now()
	u := &domain.IncidentUpdate{ID: uuid.New(), IncidentID: i.ID, Status: in.Status, MessageFR: trim(in.MessageFR),
		MessageEN: trim(in.MessageEN), CreatedBy: by, CreatedAt: now}
	i.Status, i.UpdatedBy, i.UpdatedAt = in.Status, by, now
	if i.Resolved() {
		i.ResolvedAt = &now
	}
	if err := s.Store.AddUpdate(ctx, i, u); err != nil {
		return nil, err
	}
	s.invalidate()
	return s.Store.Incident(ctx, id)
}

// ResolveInput : message de clôture (par défaut : « Incident résolu. »).
type ResolveInput struct {
	MessageFR string `json:"message_fr"`
	MessageEN string `json:"message_en"`
}

// Message de clôture par défaut (traduit par le catalogue).
const defaultResolved = "Incident résolu."

// Resolve : clôt l'incident.
func (s *Service) Resolve(ctx context.Context, id uuid.UUID, in ResolveInput, by string) (*domain.Incident, error) {
	fr, en := trim(in.MessageFR), trim(in.MessageEN)
	if fr == "" && en == "" {
		fr, en = defaultResolved, i18n.Translate(i18n.EN, defaultResolved)
	}
	return s.AddUpdate(ctx, id, domain.UpdateInput{Status: domain.StatusResolved, MessageFR: fr, MessageEN: en}, by)
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
