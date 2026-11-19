package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
	"github.com/thekrauss/kubemanager/internal/modules/billing/xykpay"
)

// Phase 3 (ADR 0012) : wallet par organisation, débit de l'usage, recharges
// par XYKPay, crédits offerts, alertes de solde.

// Payments : rail de paiement (xykpay.Client).
type Payments interface {
	Create(ctx context.Context, ch xykpay.Charge, key string) (*xykpay.Payment, error)
	Get(ctx context.Context, id string) (*xykpay.Payment, error)
}

// BalanceAlerter : alerte e-mail aux propriétaires de l'organisation.
type BalanceAlerter interface {
	BalanceAlert(ctx context.Context, org uuid.UUID, status string, balance int64, daysLeft *int64, currency string)
}

// DefaultWalletEvery : fréquence du débit, des relances de recharge et de
// l'expiration des crédits (worker).
const DefaultWalletEvery = 5 * time.Minute

// topupSyncAfter : une recharge sans webhook est relue après ce délai.
const topupSyncAfter = 30 * time.Second

// ── Wallet ──────────────────────────────────────────────────────────────

// Wallet : solde et état d'une organisation.
func (s *Service) Wallet(ctx context.Context, org uuid.UUID) (*domain.Wallet, error) {
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	c, err := s.catalog(ctx, org)
	if err != nil {
		return nil, err
	}
	cash, credit, err := s.Ledger.Balances(ctx, org)
	if err != nil {
		return nil, err
	}
	now := s.now()
	grants, err := s.Ledger.ActiveCredits(ctx, org, now)
	if err != nil {
		return nil, err
	}
	perDay, err := s.perDay(ctx, org, now)
	if err != nil {
		return nil, err
	}
	balance := cash + credit
	w := &domain.Wallet{Currency: c.Currency, Charging: acc.Charging, ChargingSince: acc.ChargingSince,
		Cash: cash.Units(), Credit: credit.Units(), Balance: balance.Units(), Credits: []domain.CreditView{},
		PerDay: perDay.Units(), DaysLeft: domain.DaysLeft(balance, perDay),
		Status: domain.WalletStatus(acc.Charging, balance, perDay), TopupMin: c.TopupMin.Units(), TopupMax: c.TopupMax.Units(),
		SuspendedAt: acc.SuspendedAt}
	if acc.Charging && acc.GraceStartedAt != nil && acc.SuspendedAt == nil {
		until := domain.GraceUntil(*acc.GraceStartedAt, c.GraceDays)
		w.GraceUntil = &until
	}
	for _, g := range grants {
		w.Credits = append(w.Credits, domain.CreditView{Amount: g.Amount.Units(), Remaining: g.Remaining.Units(), ExpiresAt: g.ExpiresAt, Reason: g.Reason})
	}
	return w, nil
}

// perDay : coût de l'heure en cours (sinon de la précédente) × 24.
func (s *Service) perDay(ctx context.Context, org uuid.UUID, now time.Time) (domain.Milli, error) {
	for _, h := range []time.Time{now.Truncate(time.Hour), now.Truncate(time.Hour).Add(-time.Hour)} {
		amounts, err := s.Store.HourAmounts(ctx, org, h)
		if err != nil {
			return 0, err
		}
		var sum domain.Milli
		for _, byMeter := range amounts {
			for _, a := range byMeter {
				sum += a
			}
		}
		if sum > 0 {
			return sum * 24, nil
		}
	}
	return 0, nil
}

// maxPage : mouvements par page au plus.
const maxPage = 200

// Transactions : historique ; les débits d'usage d'un même jour sont réunis
// en une ligne (un débit par heure serait illisible).
func (s *Service) Transactions(ctx context.Context, org uuid.UUID, before string, limit int) (*domain.TransactionsResponse, error) {
	if limit <= 0 || limit > maxPage {
		limit = 50
	}
	cur, err := repository.ParseCursor(before)
	if err != nil {
		return nil, domain.ErrInvalidCursor
	}
	rows, err := s.Ledger.Transactions(ctx, org, cur, limit)
	if err != nil {
		return nil, err
	}
	out := &domain.TransactionsResponse{Transactions: []domain.TransactionView{}}
	for _, r := range rows {
		n := len(out.Transactions)
		if r.Kind == domain.TxUsage && n > 0 {
			last := &out.Transactions[n-1]
			if last.Kind == domain.TxUsage && sameDay(last.CreatedAt, r.CreatedAt) {
				last.Amount += r.Amount
				last.Cash += r.Cash
				last.Credit += r.Credit
				continue
			}
		}
		out.Transactions = append(out.Transactions, r)
	}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		id, _ := uuid.Parse(last.ID)
		out.NextBefore = repository.Cursor{At: last.CreatedAt, ID: id}.String()
	}
	return out, nil
}

func sameDay(a, b time.Time) bool {
	ya, ma, da := a.UTC().Date()
	yb, mb, db := b.UTC().Date()
	return ya == yb && ma == mb && da == db
}

// ── Recharges ───────────────────────────────────────────────────────────

// CreateTopup : demande poussée sur le téléphone ; l'argent est crédité à la
// confirmation (webhook, ou relecture par le worker).
func (s *Service) CreateTopup(ctx context.Context, org uuid.UUID, req domain.TopupRequest, by string) (*domain.TopupView, error) {
	if s.Payments == nil {
		return nil, domain.ErrPaymentsDisabled
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	if !acc.Charging {
		return nil, domain.ErrChargingDisabled
	}
	c, err := s.catalog(ctx, org)
	if err != nil {
		return nil, err
	}
	if c.TopupMax <= 0 {
		return nil, domain.ErrTopupsClosed
	}
	amount := domain.FromUnits(req.Amount)
	if amount < c.TopupMin || amount > c.TopupMax {
		return nil, &domain.TopupAmountError{Min: c.TopupMin.Units(), Max: c.TopupMax.Units(), Currency: c.Currency}
	}
	phone := domain.NormalizePhone(req.Phone)
	if phone == "" {
		return nil, domain.ErrInvalidPhone
	}
	if !domain.ValidOperator(req.Operator) {
		return nil, domain.ErrInvalidOperator
	}
	now := s.now()
	if pending, err := s.Ledger.HasPendingTopup(ctx, org, now.Add(-domain.TopupExpiry)); err != nil {
		return nil, err
	} else if pending {
		return nil, domain.ErrTopupPending
	}
	t := &domain.Topup{ID: uuid.New(), OrganizationID: org, Amount: amount, Currency: c.Currency, Phone: phone,
		Operator: req.Operator, Status: domain.TopupPending, CreatedBy: by, CreatedAt: now, UpdatedAt: now}
	if err := s.Ledger.CreateTopup(ctx, t); err != nil {
		return nil, err
	}
	p, err := s.Payments.Create(ctx, xykpay.Charge{Amount: req.Amount, Currency: c.Currency, Phone: phone, Operator: req.Operator,
		Description: "Recharge betorun", Reference: domain.TopupReference(t.ID)}, t.ID.String())
	if err != nil {
		code := "PAYMENT_UNAVAILABLE"
		var apiErr *xykpay.APIError
		if errors.As(err, &apiErr) && apiErr.Code != "" {
			code = apiErr.Code
		}
		s.log().Warnw("billing: top-up refused by the payment provider", "topup", t.ID, "error", err)
		if _, serr := s.Ledger.SettleTopup(ctx, t.ID, domain.TopupFailed, code, now); serr != nil {
			s.log().Warnw("billing: failed top-up not recorded", "topup", t.ID, "error", serr)
		}
		return nil, &domain.PaymentRefusedError{Code: code}
	}
	if err := s.Ledger.SetTopupPayment(ctx, t.ID, p.ID, p.Environment); err != nil {
		return nil, err
	}
	pid := p.ID
	t.PaymentID, t.Environment = &pid, p.Environment
	// Paiement de test réglé aussitôt, ou déjà refusé : l'issue est connue.
	if status := topupStatus(p); status != domain.TopupPending {
		settled, err := s.Ledger.SettleTopup(ctx, t.ID, status, p.FailureCode, now)
		if err != nil {
			return nil, err
		}
		t = settled
		s.afterTopup(ctx, t)
	}
	v := t.View()
	return &v, nil
}

// topupStatus : état d'une recharge selon le paiement.
func topupStatus(p *xykpay.Payment) string {
	switch p.Status {
	case "succeeded":
		return domain.TopupSucceeded
	case "failed", "cancelled", "canceled", "expired":
		return domain.TopupFailed
	default:
		return domain.TopupPending
	}
}

// Topup : une recharge, relue chez XYKPay si elle attend encore.
func (s *Service) Topup(ctx context.Context, org, id uuid.UUID) (*domain.TopupView, error) {
	t, err := s.Ledger.Topup(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if !t.Done() && t.PaymentID != nil && s.Payments != nil {
		if synced, err := s.syncTopup(ctx, t); err != nil {
			s.log().Warnw("billing: top-up sync failed", "topup", t.ID, "error", err)
		} else {
			t = synced
		}
	}
	v := t.View()
	return &v, nil
}

// Topups : recharges récentes.
func (s *Service) Topups(ctx context.Context, org uuid.UUID) (*domain.TopupsResponse, error) {
	rows, err := s.Ledger.RecentTopups(ctx, org, 20)
	if err != nil {
		return nil, err
	}
	out := &domain.TopupsResponse{Topups: make([]domain.TopupView, 0, len(rows))}
	for _, t := range rows {
		out.Topups = append(out.Topups, t.View())
	}
	return out, nil
}

// syncTopup : relit le paiement et règle la recharge ; une demande restée
// sans réponse au-delà de TopupExpiry expire (elle sera encore créditée si
// le paiement réussit ensuite).
func (s *Service) syncTopup(ctx context.Context, t *domain.Topup) (*domain.Topup, error) {
	p, err := s.Payments.Get(ctx, *t.PaymentID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	status := topupStatus(p)
	if status == domain.TopupPending {
		if now.Sub(t.CreatedAt) < domain.TopupExpiry || t.Status == domain.TopupExpired {
			return t, nil
		}
		status = domain.TopupExpired
	}
	wasPaid := t.Status == domain.TopupSucceeded
	settled, err := s.Ledger.SettleTopup(ctx, t.ID, status, p.FailureCode, now)
	if err == nil && !wasPaid {
		s.afterTopup(ctx, settled)
	}
	return settled, err
}

// HandleWebhook : signal de XYKPay ; le paiement est relu avant tout crédit.
func (s *Service) HandleWebhook(ctx context.Context, signature string, body []byte) error {
	if s.Payments == nil || s.WebhookSecret == "" {
		return domain.ErrPaymentsDisabled
	}
	if err := xykpay.Verify(s.WebhookSecret, signature, body, s.now()); err != nil {
		return domain.ErrBadWebhook
	}
	id := xykpay.PaymentID(body)
	if id == "" {
		return nil // autre événement : ignoré
	}
	t, err := s.Ledger.TopupByPayment(ctx, id)
	if errors.Is(err, domain.ErrTopupNotFound) {
		return nil // paiement d'un autre usage du compte
	}
	if err != nil {
		return err
	}
	if t.Status == domain.TopupSucceeded {
		return nil
	}
	_, err = s.syncTopup(ctx, t)
	return err
}

// syncPendingTopups : filet du worker, pour les webhooks perdus.
func (s *Service) syncPendingTopups(ctx context.Context) {
	if s.Payments == nil {
		return
	}
	rows, err := s.Ledger.PendingTopups(ctx, s.now().Add(-topupSyncAfter), 100)
	if err != nil {
		s.log().Warnw("billing: pending top-ups lookup failed", "error", err)
		return
	}
	for i := range rows {
		if rows[i].PaymentID == nil {
			// Création interrompue avant la réponse de XYKPay.
			if s.now().Sub(rows[i].CreatedAt) > domain.TopupExpiry {
				_, _ = s.Ledger.SettleTopup(ctx, rows[i].ID, domain.TopupExpired, "", s.now())
			}
			continue
		}
		if _, err := s.syncTopup(ctx, &rows[i]); err != nil {
			s.log().Warnw("billing: top-up sync failed", "topup", rows[i].ID, "error", err)
		}
	}
}

// ── Débit de l'usage ────────────────────────────────────────────────────

// Charge : pour chaque organisation en facturation réelle, débite ce que
// coûte le mois jusqu'à la dernière heure terminée (plafond et offre gratuite
// compris), moins ce qui a déjà été débité. Le débit horaire est une
// différence : un relevé tardif (build terminé en fin d'heure) est rattrapé
// à l'heure suivante, rien n'est compté deux fois. Rend le nombre de débits.
func (s *Service) Charge(ctx context.Context) (int, error) {
	orgs, err := s.Ledger.ChargingOrgs(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, org := range orgs {
		charged, err := s.chargeOrg(ctx, org)
		if err != nil {
			s.log().Warnw("billing: usage charge failed", "organization", org, "error", err)
			continue
		}
		if charged > 0 {
			n++
		}
		if err := s.settleAccount(ctx, org); err != nil {
			s.log().Warnw("billing: grace or suspension check failed", "organization", org, "error", err)
		}
		s.checkBalance(ctx, org)
	}
	return n, nil
}

func (s *Service) chargeOrg(ctx context.Context, org uuid.UUID) (domain.Milli, error) {
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil || !acc.Charging {
		return 0, err
	}
	c, err := s.catalog(ctx, org)
	if err != nil {
		return 0, err
	}
	// Dernière heure terminée ; son mois est la période débitée.
	last := s.now().Truncate(time.Hour).Add(-time.Hour)
	month := time.Date(last.Year(), last.Month(), 1, 0, 0, 0, 0, time.UTC)
	from := month
	// L'usage d'avant l'activation reste une estimation : il n'est pas débité.
	if acc.ChargingSince != nil {
		if since := acc.ChargingSince.UTC().Truncate(time.Hour); since.After(from) {
			from = since
		}
	}
	if !from.Before(last.Add(time.Hour)) {
		return 0, nil
	}
	totals, err := s.Store.Totals(ctx, org, from, last.Add(time.Hour))
	if err != nil {
		return 0, err
	}
	period := month.Format("2006-01")
	return s.Ledger.ChargeUsage(ctx, org, period, c.NetCost(totals), domain.UsageReference(org, last), c.Currency,
		"Usage "+period, s.now())
}

// checkBalance : alerte e-mail quand le solde franchit un seuil (7 jours,
// 2 jours, épuisé) ; une seule fois par seuil, jusqu'à ce qu'il remonte.
func (s *Service) checkBalance(ctx context.Context, org uuid.UUID) {
	w, err := s.Wallet(ctx, org)
	if err != nil {
		s.log().Warnw("billing: balance check failed", "organization", org, "error", err)
		return
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return
	}
	level := domain.AlertLevel(w.Status)
	if level == acc.AlertLevel {
		return
	}
	if err := s.Ledger.SetAlertLevel(ctx, org, level); err != nil {
		s.log().Warnw("billing: alert level not recorded", "organization", org, "error", err)
		return
	}
	if level > acc.AlertLevel && s.Alerts != nil {
		s.Alerts.BalanceAlert(ctx, org, w.Status, w.Balance, w.DaysLeft, w.Currency)
	}
}

// RunWallet : boucle du worker (débit, recharges en attente, crédits expirés).
func (s *Service) RunWallet(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultWalletEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		s.syncPendingTopups(ctx)
		if n, err := s.Ledger.ExpireCredits(ctx, s.now()); err != nil {
			s.log().Warnw("billing: credit expiry failed", "error", err)
		} else if n > 0 {
			s.log().Infow("billing: credits expired", "count", n)
		}
		if _, err := s.Charge(ctx); err != nil {
			s.log().Warnw("billing: charge failed", "error", err)
		}
		s.checkBudgets(ctx)
		s.IssueDocuments(ctx)
		// Limites des projets au niveau de leur plan (nouveau projet, plan modifié).
		if applied, missed, err := s.ReconcileLimits(ctx, nil, nil); err != nil {
			s.log().Warnw("billing: limits reconciliation failed", "error", err)
		} else if applied > 0 || len(missed) > 0 {
			s.log().Infow("billing: project limits reconciled", "updated", applied, "pending", len(missed))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
