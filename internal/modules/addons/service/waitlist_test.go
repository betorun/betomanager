package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
)

type fakeWaitlist struct {
	rows     map[string]*domain.WaitlistEntry
	pending  map[string][]domain.Subscriber
	notified []string
}

func key(u uuid.UUID, t string) string { return u.String() + "/" + t }

func (f *fakeWaitlist) Join(_ context.Context, e *domain.WaitlistEntry) error {
	if f.rows == nil {
		f.rows = map[string]*domain.WaitlistEntry{}
	}
	if _, ok := f.rows[key(e.UserID, e.Template)]; !ok {
		f.rows[key(e.UserID, e.Template)] = e
	}
	return nil
}
func (f *fakeWaitlist) Leave(_ context.Context, u uuid.UUID, t string) error {
	delete(f.rows, key(u, t))
	return nil
}
func (f *fakeWaitlist) Mine(_ context.Context, u uuid.UUID) ([]string, error) {
	var out []string
	for _, e := range f.rows {
		if e.UserID == u && e.NotifiedAt == nil {
			out = append(out, e.Template)
		}
	}
	return out, nil
}
func (f *fakeWaitlist) Pending(_ context.Context, t string, _ int) ([]domain.Subscriber, error) {
	return f.pending[t], nil
}
func (f *fakeWaitlist) MarkNotified(_ context.Context, u uuid.UUID, t string) error {
	f.notified = append(f.notified, key(u, t))
	return nil
}

type fakeAnnouncer struct {
	sent []string
	fail string
}

func (a *fakeAnnouncer) AddonAvailable(_ context.Context, lang i18n.Locale, email, _, title string) error {
	if email == a.fail {
		return errors.New("smtp down")
	}
	a.sent = append(a.sent, string(lang)+":"+email+":"+title)
	return nil
}

func TestJoinWaitlistOnlyAnnouncedServices(t *testing.T) {
	w := &fakeWaitlist{}
	s := &Service{Waitlists: w}
	u := uuid.New()
	ctx := i18n.With(context.Background(), i18n.EN)
	// Service annoncé de test (le catalogue réel peut n'en annoncer aucun).
	saved := domain.Announced
	domain.Announced = []domain.Upcoming{{ID: "soon", Title: "Soon", Logo: "soon", Category: "search"}}
	t.Cleanup(func() { domain.Announced = saved })

	for _, open := range []string{"redis", "mariadb", "meilisearch"} {
		if err := s.JoinWaitlist(ctx, u, open); !errors.Is(err, domain.ErrNotAnnounced) {
			t.Fatalf("%s is already open, want ErrNotAnnounced, got %v", open, err)
		}
	}
	if err := s.JoinWaitlist(ctx, u, "nope"); !errors.Is(err, domain.ErrNotAnnounced) {
		t.Fatalf("unknown service, want ErrNotAnnounced, got %v", err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := s.JoinWaitlist(ctx, u, "soon"); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.rows[key(u, "soon")].Locale; got != "en" {
		t.Fatalf("locale of the request must be kept, got %q", got)
	}
	v, err := s.Waitlist(ctx, u)
	if err != nil || len(v.Templates) != 1 || v.Templates[0] != "soon" {
		t.Fatalf("waitlist = %+v, %v", v, err)
	}
	if err := s.LeaveWaitlist(ctx, u, "soon"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Waitlist(ctx, u); len(v.Templates) != 0 {
		t.Fatalf("left, still %v", v.Templates)
	}
}

func TestNotifyOpenedSendsOnceAndRetriesFailures(t *testing.T) {
	ok, ko := uuid.New(), uuid.New()
	w := &fakeWaitlist{pending: map[string][]domain.Subscriber{
		"redis": {
			{UserID: ok, Email: "a@x.test", Locale: "en"},
			{UserID: ko, Email: "b@x.test", Locale: "fr"},
		},
	}}
	a := &fakeAnnouncer{fail: "b@x.test"}
	s := &Service{Waitlists: w, Announcer: a}
	s.NotifyOpened(context.Background())

	if len(a.sent) != 1 || a.sent[0] != "en:a@x.test:Redis (via Valkey)" {
		t.Fatalf("sent = %v", a.sent)
	}
	// Échec d'envoi : pas marqué, retenté au passage suivant.
	if len(w.notified) != 1 || w.notified[0] != key(ok, "redis") {
		t.Fatalf("notified = %v", w.notified)
	}
}

func TestRunWaitlistNotifierStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&Service{Waitlists: &fakeWaitlist{}, Announcer: &fakeAnnouncer{}}).RunWaitlistNotifier(ctx, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifier did not stop")
	}
}
