package addons

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
)

// « Me prévenir » (ADR 0013) : un utilisateur demande à être prévenu quand un
// service annoncé « Bientôt » ouvre. À l'ouverture (le modèle entre dans
// domain.Catalog), chaque demande reçoit UN e-mail, dans sa langue.

// WaitlistStore : demandes « Me prévenir ».
type WaitlistStore interface {
	Join(ctx context.Context, e *domain.WaitlistEntry) error
	Leave(ctx context.Context, user uuid.UUID, template string) error
	// Mine : services pour lesquels l'utilisateur attend encore l'e-mail.
	Mine(ctx context.Context, user uuid.UUID) ([]string, error)
	// Pending : demandes pas encore prévenues d'un service (comptes actifs).
	Pending(ctx context.Context, template string, limit int) ([]domain.Subscriber, error)
	MarkNotified(ctx context.Context, user uuid.UUID, template string) error
}

// Announcer : envoi de l'e-mail « le service est disponible ».
type Announcer interface {
	AddonAvailable(ctx context.Context, lang i18n.Locale, email, fullName, title string) error
}

// WaitlistEvery : fréquence des passages (un service ouvre à une mise en
// production : une heure de délai au plus suffit).
const WaitlistEvery = time.Hour

// notifyBatch : envois par passage (le reste part au passage suivant).
const notifyBatch = 200

// Waitlist : services annoncés que l'utilisateur attend.
func (s *Service) Waitlist(ctx context.Context, user uuid.UUID) (*domain.WaitlistView, error) {
	out := &domain.WaitlistView{Templates: []string{}}
	if s.Waitlists == nil {
		return out, nil
	}
	mine, err := s.Waitlists.Mine(ctx, user)
	if err != nil {
		return nil, err
	}
	for _, t := range mine {
		// Déjà ouvert : l'e-mail part au prochain passage, plus rien à attendre.
		if _, ok := domain.UpcomingByID(t); ok {
			out.Templates = append(out.Templates, t)
		}
	}
	return out, nil
}

// JoinWaitlist : prévenir l'utilisateur à l'ouverture (idempotent).
func (s *Service) JoinWaitlist(ctx context.Context, user uuid.UUID, template string) error {
	if _, ok := domain.UpcomingByID(template); !ok {
		return domain.ErrNotAnnounced
	}
	if s.Waitlists == nil {
		return domain.ErrUnavailable
	}
	lang := i18n.From(ctx)
	if lang != i18n.EN {
		lang = i18n.FR
	}
	return s.Waitlists.Join(ctx, &domain.WaitlistEntry{UserID: user, Template: template, Locale: string(lang)})
}

// LeaveWaitlist : ne plus être prévenu (idempotent).
func (s *Service) LeaveWaitlist(ctx context.Context, user uuid.UUID, template string) error {
	if _, ok := domain.UpcomingByID(template); !ok {
		return domain.ErrNotAnnounced
	}
	if s.Waitlists == nil {
		return nil
	}
	return s.Waitlists.Leave(ctx, user, template)
}

// NotifyOpened : prévient les demandes des services désormais ouverts. Un
// e-mail raté est retenté au passage suivant (notified_at reste vide).
func (s *Service) NotifyOpened(ctx context.Context) {
	if s.Waitlists == nil || s.Announcer == nil {
		return
	}
	for _, t := range domain.Catalog {
		subs, err := s.Waitlists.Pending(ctx, t.ID, notifyBatch)
		if err != nil {
			s.warn("addon waitlist: pending lookup failed", "template", t.ID, "error", err)
			continue
		}
		for _, sub := range subs {
			lang := i18n.FR
			if sub.Locale == string(i18n.EN) {
				lang = i18n.EN
			}
			if err := s.Announcer.AddonAvailable(ctx, lang, sub.Email, sub.FullName, t.Title); err != nil {
				s.warn("addon waitlist: email failed", "template", t.ID, "error", err)
				continue
			}
			if err := s.Waitlists.MarkNotified(ctx, sub.UserID, t.ID); err != nil {
				s.warn("addon waitlist: mark failed", "template", t.ID, "error", err)
			}
		}
	}
}

// RunWaitlistNotifier : un passage au démarrage, puis toutes les `every`.
func (s *Service) RunWaitlistNotifier(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.NotifyOpened(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// GormWaitlist : demandes en base (table addon_waitlist).
type GormWaitlist struct{ DB *gorm.DB }

func (w *GormWaitlist) Join(ctx context.Context, e *domain.WaitlistEntry) error {
	return w.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(e).Error
}

func (w *GormWaitlist) Leave(ctx context.Context, user uuid.UUID, template string) error {
	return w.DB.WithContext(ctx).Where("user_id = ? AND template = ?", user, template).Delete(&domain.WaitlistEntry{}).Error
}

func (w *GormWaitlist) Mine(ctx context.Context, user uuid.UUID) ([]string, error) {
	var out []string
	err := w.DB.WithContext(ctx).Model(&domain.WaitlistEntry{}).
		Where("user_id = ? AND notified_at IS NULL", user).Order("template").Pluck("template", &out).Error
	return out, err
}

func (w *GormWaitlist) Pending(ctx context.Context, template string, limit int) ([]domain.Subscriber, error) {
	var out []domain.Subscriber
	err := w.DB.WithContext(ctx).Raw(`
		SELECT w.user_id, u.email, u.full_name, w.locale
		  FROM addon_waitlist w
		  JOIN users u ON u.id = w.user_id
		 WHERE w.template = ? AND w.notified_at IS NULL AND u.deleted_at IS NULL AND u.is_active
		 ORDER BY w.created_at
		 LIMIT ?`, template, limit).Scan(&out).Error
	return out, err
}

func (w *GormWaitlist) MarkNotified(ctx context.Context, user uuid.UUID, template string) error {
	return w.DB.WithContext(ctx).Model(&domain.WaitlistEntry{}).
		Where("user_id = ? AND template = ?", user, template).Update("notified_at", time.Now()).Error
}
