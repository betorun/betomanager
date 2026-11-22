package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/pdf"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

// Phase 4 (ADR 0012) : grâce et suspension douce, reprise automatique,
// budgets par projet, factures et reçus, crédit d'essai par numéro.

// Containers : arrêt et redémarrage d'un conteneur (service des conteneurs).
type Containers interface {
	Stop(ctx context.Context, id uuid.UUID) error
	Start(ctx context.Context, id uuid.UUID) error
}

// VPSSuspender : VPS de l'organisation (module compute), arrêtés pendant la
// suspension et relancés à sa levée.
type VPSSuspender interface {
	SuspendOrganization(ctx context.Context, org uuid.UUID) error
	ResumeOrganization(ctx context.Context, org uuid.UUID) error
}

// BudgetAlerter : alerte e-mail aux owners d'un projet (80 %, 100 %).
type BudgetAlerter interface {
	BudgetAlert(ctx context.Context, project uuid.UUID, level int, spent, limit int64, currency string, stopped bool)
}

// invoiceDelay : la facture d'un mois part une fois sa dernière heure
// débitée (débit toutes les 5 min, relevés en retard compris).
const invoiceDelay = 2 * time.Hour

// ── Grâce, suspension, reprise ──────────────────────────────────────────

// settleAccount : après un débit ou un crédit. Solde positif : fin de la
// grâce, et reprise si l'organisation était suspendue. Solde épuisé : début
// de la grâce, puis suspension douce quand elle est écoulée (ou la dette trop
// grosse).
func (s *Service) settleAccount(ctx context.Context, org uuid.UUID) error {
	if s.Phase4 == nil {
		return nil
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil || !acc.Charging {
		return err
	}
	cash, credit, err := s.Ledger.Balances(ctx, org)
	if err != nil {
		return err
	}
	// Postpayé : la dette compte jusqu'au plafond, et une facture impayée
	// trop longtemps suspend aussi.
	balance, overdue, err := s.postpaidState(ctx, org, acc, cash+credit, cash)
	if err != nil {
		return err
	}
	now := s.now()
	if overdue {
		if acc.SuspendedAt == nil {
			return s.suspend(ctx, org)
		}
		return nil
	}
	if balance > 0 {
		if acc.GraceStartedAt != nil {
			if err := s.Phase4.SetGrace(ctx, org, nil); err != nil {
				return err
			}
		}
		if acc.SuspendedAt != nil {
			return s.resume(ctx, org)
		}
		return nil
	}
	if acc.GraceStartedAt == nil {
		return s.Phase4.SetGrace(ctx, org, &now)
	}
	if acc.SuspendedAt != nil {
		return nil
	}
	c, err := s.catalog(ctx, org)
	if err != nil {
		return err
	}
	perDay, err := s.perDay(ctx, org, now)
	if err != nil {
		return err
	}
	if domain.ShouldSuspend(balance, perDay, acc.GraceStartedAt, c.GraceDays, now) {
		return s.suspend(ctx, org)
	}
	return nil
}

// suspend : arrête les conteneurs en marche de l'organisation et retient
// lesquels, pour ne redémarrer qu'eux. Les données (disques, bases) restent.
func (s *Service) suspend(ctx context.Context, org uuid.UUID) error {
	now := s.now()
	// Marquée suspendue d'abord : plus rien ne redémarre pendant l'arrêt.
	if err := s.Phase4.SetSuspended(ctx, org, &now); err != nil {
		return err
	}
	stopped, err := s.stopAll(ctx, org, nil, domain.StopBalance)
	if s.VPS != nil {
		if verr := s.VPS.SuspendOrganization(ctx, org); verr != nil {
			s.log().Warnw("billing: VPS not all suspended", "organization", org, "error", verr)
			if err == nil {
				err = verr
			}
		}
	}
	s.log().Warnw("billing: organization suspended (balance used up, grace over)", "organization", org, "stopped", stopped)
	if s.Alerts != nil {
		w, werr := s.Wallet(ctx, org)
		if werr == nil {
			s.Alerts.BalanceAlert(ctx, org, "suspended", w.Balance, nil, w.Currency)
		}
	}
	return err
}

// stopAll : arrête les conteneurs en marche (d'un projet ou de toute
// l'organisation) ; un échec n'empêche pas d'arrêter les autres.
func (s *Service) stopAll(ctx context.Context, org uuid.UUID, project *uuid.UUID, reason string) (int, error) {
	if s.Containers == nil {
		return 0, nil
	}
	running, err := s.Inventory.RunningWorkloads(ctx, org, project)
	if err != nil {
		return 0, err
	}
	n := 0
	var firstErr error
	for _, w := range running {
		if err := s.Containers.Stop(ctx, w.ID); err != nil {
			s.log().Warnw("billing: container not stopped", "workload", w.ID, "reason", reason, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := s.Phase4.RecordStopped(ctx, org, w.ProjectID, w.ID, reason, s.now()); err != nil && firstErr == nil {
			firstErr = err
		}
		n++
	}
	return n, firstErr
}

// resume : fin de la suspension, redémarrage des conteneurs arrêtés par elle
// (sauf ceux d'un projet dont le budget les retient).
func (s *Service) resume(ctx context.Context, org uuid.UUID) error {
	if err := s.Phase4.SetSuspended(ctx, org, nil); err != nil {
		return err
	}
	ids, err := s.Phase4.Stopped(ctx, org, nil, domain.StopBalance)
	if err != nil {
		return err
	}
	s.startAll(ctx, ids)
	if s.VPS != nil {
		if err := s.VPS.ResumeOrganization(ctx, org); err != nil {
			s.log().Warnw("billing: VPS not all resumed", "organization", org, "error", err)
		}
	}
	s.log().Infow("billing: organization resumed", "organization", org, "restarted", len(ids))
	return nil
}

func (s *Service) startAll(ctx context.Context, ids []uuid.UUID) {
	for _, id := range ids {
		if s.Containers != nil {
			if err := s.Containers.Start(ctx, id); err != nil {
				// Supprimé ou modifié entre-temps : il n'est plus à relancer.
				s.log().Warnw("billing: container not restarted", "workload", id, "error", err)
			}
		}
		if err := s.Phase4.ClearStopped(ctx, id); err != nil {
			s.log().Warnw("billing: stopped container not cleared", "workload", id, "error", err)
		}
	}
}

// unavailable : refus « réessayez » (503) ; la cause reste dans la chaîne
// d'erreurs pour les journaux, jamais dans la réponse.
func unavailable(cause error) error {
	return fmt.Errorf("%w: %w", domain.ErrBillingCheckUnavailable, cause)
}

// CanRun : garde du service des conteneurs. Refuse de démarrer ou déployer
// dans une organisation suspendue, ou dans un projet arrêté par son budget.
func (s *Service) CanRun(ctx context.Context, project uuid.UUID) error {
	if s.Phase4 == nil {
		return nil
	}
	org, err := s.Inventory.ProjectOrg(ctx, project)
	if errors.Is(err, domain.ErrProjectUnknown) {
		return nil // projet inconnu ici : le service des conteneurs répondra
	}
	if err != nil {
		return unavailable(err) // panne de lecture : la garde reste fermée
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return unavailable(err)
	}
	if acc.Charging && acc.SuspendedAt != nil {
		return domain.ErrAccountSuspended
	}
	b, err := s.Phase4.Budget(ctx, project)
	if err != nil {
		return unavailable(err)
	}
	if b != nil && b.Stop && b.AlertLevel >= domain.BudgetAlertOver && b.Period == s.now().Format("2006-01") {
		return domain.ErrAccountSuspended
	}
	return nil
}

// ── Budgets par projet ──────────────────────────────────────────────────

// projectSpent : coût du projet ce mois-ci (avant offre gratuite, qui est
// celle de l'organisation).
func (s *Service) projectSpent(ctx context.Context, org, project uuid.UUID, now time.Time) (domain.Milli, error) {
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	totals, err := s.Store.Totals(ctx, org, month, month.AddDate(0, 1, 0))
	if err != nil {
		return 0, err
	}
	var spent domain.Milli
	for _, t := range totals {
		if t.ProjectID == project {
			spent += t.Cost()
		}
	}
	return spent, nil
}

// Budget : budget du projet et sa consommation du mois.
func (s *Service) Budget(ctx context.Context, project uuid.UUID) (*domain.Budget, error) {
	org, err := s.Inventory.ProjectOrg(ctx, project)
	if err != nil {
		return nil, err
	}
	c, err := s.catalog(ctx, org)
	if err != nil {
		return nil, err
	}
	now := s.now()
	spent, err := s.projectSpent(ctx, org, project, now)
	if err != nil {
		return nil, err
	}
	out := &domain.Budget{ProjectID: project.String(), Spent: spent.Units(), Currency: c.Currency}
	b, err := s.Phase4.Budget(ctx, project)
	if err != nil {
		return nil, err
	}
	if b != nil {
		out.Limit, out.Stop = b.MonthlyLimit.Units(), b.Stop
		out.Stopped = b.Stop && b.AlertLevel >= domain.BudgetAlertOver && b.Period == now.Format("2006-01")
	}
	return out, nil
}

// SetBudget : fixe ou retire le budget ; relevé ou retiré, les conteneurs
// que le budget avait arrêtés redémarrent (si l'organisation n'est pas
// suspendue).
func (s *Service) SetBudget(ctx context.Context, project uuid.UUID, req domain.BudgetRequest, by string) (*domain.Budget, error) {
	if req.Limit < 0 {
		return nil, domain.ErrInvalidBudget
	}
	org, err := s.Inventory.ProjectOrg(ctx, project)
	if err != nil {
		return nil, err
	}
	if req.Limit == 0 {
		if err := s.Phase4.DeleteBudget(ctx, project); err != nil {
			return nil, err
		}
	} else if err := s.Phase4.SetBudget(ctx, repository.BudgetRow{ProjectID: project, OrganizationID: org,
		MonthlyLimit: domain.FromUnits(req.Limit), Stop: req.Stop, UpdatedBy: by}); err != nil {
		return nil, err
	}
	if err := s.checkBudget(ctx, project); err != nil {
		return nil, err
	}
	return s.Budget(ctx, project)
}

// checkBudget : seuils d'un budget pour le mois en cours ; nouveau mois ou
// budget relevé : les conteneurs arrêtés par le budget redémarrent.
func (s *Service) checkBudget(ctx context.Context, project uuid.UUID) error {
	org, err := s.Inventory.ProjectOrg(ctx, project)
	if err != nil {
		return err
	}
	b, err := s.Phase4.Budget(ctx, project)
	if err != nil {
		return err
	}
	now := s.now()
	period := now.Format("2006-01")
	level, stored := domain.BudgetAlertNone, domain.BudgetAlertNone
	var spent domain.Milli
	if b != nil {
		if spent, err = s.projectSpent(ctx, org, project, now); err != nil {
			return err
		}
		level = domain.BudgetLevel(spent, b.MonthlyLimit)
		if b.Period == period {
			stored = b.AlertLevel
		}
	}
	// Seuil enregistré d'abord : la garde des conteneurs le lit.
	if b != nil && (level != stored || b.Period != period) {
		if err := s.Phase4.SetBudgetAlert(ctx, project, level, period); err != nil {
			return err
		}
	}
	// Plus de dépassement (nouveau mois, budget relevé, retiré ou sans arrêt).
	if b == nil || !b.Stop || level < domain.BudgetAlertOver {
		if err := s.releaseBudget(ctx, org, project); err != nil {
			return err
		}
	}
	if b == nil {
		return nil
	}
	if level > stored {
		stopped := false
		if level >= domain.BudgetAlertOver && b.Stop {
			n, err := s.stopAll(ctx, org, &project, domain.StopBudget)
			if err != nil {
				s.log().Warnw("billing: budget stop incomplete", "project", project, "error", err)
			}
			stopped = n > 0
		}
		if s.Budgets != nil {
			c, err := s.catalog(ctx, org)
			if err == nil {
				s.Budgets.BudgetAlert(ctx, project, level, spent.Units(), b.MonthlyLimit.Units(), c.Currency, stopped)
			}
		}
	}
	return nil
}

// releaseBudget : redémarre ce que le budget avait arrêté (pas pendant une
// suspension : la reprise s'en chargera).
func (s *Service) releaseBudget(ctx context.Context, org, project uuid.UUID) error {
	ids, err := s.Phase4.Stopped(ctx, org, &project, domain.StopBudget)
	if err != nil || len(ids) == 0 {
		return err
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return err
	}
	if acc.Charging && acc.SuspendedAt != nil {
		return nil
	}
	s.startAll(ctx, ids)
	return nil
}

// checkBudgets : tous les budgets (worker).
func (s *Service) checkBudgets(ctx context.Context) {
	if s.Phase4 == nil {
		return
	}
	budgets, err := s.Phase4.Budgets(ctx)
	if err != nil {
		s.log().Warnw("billing: budgets lookup failed", "error", err)
		return
	}
	for _, b := range budgets {
		if err := s.checkBudget(ctx, b.ProjectID); err != nil {
			s.log().Warnw("billing: budget check failed", "project", b.ProjectID, "error", err)
		}
	}
}

// ── Profil de facturation ───────────────────────────────────────────────

// Profile : informations portées sur les factures.
func (s *Service) Profile(ctx context.Context, org uuid.UUID) (*domain.Profile, error) {
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	return &acc.Profile, nil
}

// SetProfile : modifie le profil (s'applique aux prochaines factures ; les
// factures émises ne changent pas).
func (s *Service) SetProfile(ctx context.Context, org uuid.UUID, req domain.ProfileRequest) (*domain.Profile, error) {
	p, err := domain.NormalizeProfile(req)
	if err != nil {
		return nil, err
	}
	if err := s.Phase4.SetProfile(ctx, org, p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ── Factures et reçus ───────────────────────────────────────────────────

// Documents : factures et reçus de l'organisation.
func (s *Service) Documents(ctx context.Context, org uuid.UUID) (*domain.DocumentsResponse, error) {
	docs, err := s.Phase4.Documents(ctx, org, 100)
	if err != nil {
		return nil, err
	}
	out := &domain.DocumentsResponse{Documents: make([]domain.DocumentView, 0, len(docs)), IssuerReady: s.Issuer.Configured()}
	for _, d := range docs {
		out.Documents = append(out.Documents, d.View())
	}
	return out, nil
}

// DocumentPDF : le PDF archivé d'un document.
func (s *Service) DocumentPDF(ctx context.Context, org, id uuid.UUID) (*domain.Document, error) {
	return s.Phase4.Document(ctx, org, id)
}

func (s *Service) orgName(ctx context.Context, org uuid.UUID) string {
	if s.Admin == nil {
		return ""
	}
	r, err := s.Admin.Org(ctx, org)
	if err != nil {
		return ""
	}
	return r.Name
}

// IssueDocuments : reçus des recharges réussies, puis factures du mois
// écoulé (worker). Sans entité configurée, rien n'est émis.
func (s *Service) IssueDocuments(ctx context.Context) {
	s.issueReceipts(ctx)
	s.issueInvoices(ctx)
}

// issueInvoices : facture du mois précédent pour chaque organisation
// débitée, une fois ce mois entièrement débité. Idempotent.
func (s *Service) issueInvoices(ctx context.Context) {
	if s.Phase4 == nil || !s.Issuer.Configured() {
		return
	}
	now := s.now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if now.Sub(monthStart) < invoiceDelay {
		return
	}
	prev := monthStart.AddDate(0, -1, 0)
	period := prev.Format("2006-01")
	orgs, err := s.Phase4.InvoiceableOrgs(ctx, period)
	if err != nil {
		s.log().Warnw("billing: invoiceable organizations lookup failed", "error", err)
		return
	}
	for _, org := range orgs {
		if err := s.issueInvoice(ctx, org, prev); err != nil {
			s.log().Warnw("billing: invoice not issued", "organization", org, "period", period, "error", err)
		}
	}
}

func (s *Service) issueInvoice(ctx context.Context, org uuid.UUID, month time.Time) error {
	period := month.Format("2006-01")
	if exists, err := s.Phase4.InvoiceExists(ctx, org, period); err != nil || exists {
		return err
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return err
	}
	credit, cash, err := s.Phase4.UsageDebits(ctx, org, period)
	if err != nil || credit+cash <= 0 {
		return err
	}
	c, err := s.catalog(ctx, org)
	if err != nil {
		return err
	}
	// Même fenêtre que le débit : depuis l'activation, jusqu'à la fin du mois.
	from := month
	if acc.ChargingSince != nil && acc.ChargingSince.After(from) {
		from = acc.ChargingSince.UTC().Truncate(time.Hour)
	}
	totals, err := s.Store.Totals(ctx, org, from, month.AddDate(0, 1, 0))
	if err != nil {
		return err
	}
	lines := c.InvoiceLines(totals)
	var sum domain.Milli
	for _, l := range lines {
		sum += l.Amount
	}
	if diff := credit + cash - sum; diff != 0 {
		// Débit d'une heure relevée après coup, ou grille modifiée en cours
		// de mois : la facture suit ce qui a réellement été débité.
		lines = append(lines, domain.DocLine{Label: "Ajustement de mesure", Amount: diff})
	}
	vat := domain.SplitVAT(cash, s.Issuer.VATRateBP)
	data := domain.DocData{Issuer: s.Issuer, Customer: acc.Profile, OrgName: s.orgName(ctx, org), Lines: lines,
		PaidByCredits: credit, PaidByCash: cash}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	now := s.now()
	doc := &domain.Document{ID: uuid.New(), OrganizationID: org, Kind: domain.DocInvoice, Period: period, Currency: c.Currency,
		Total: cash, VAT: vat, VATRateBP: s.Issuer.VATRateBP, Data: raw, IssuedAt: now}
	// Postpayé : facture à payer avant l'échéance (et non déjà réglée).
	if acc.Postpaid {
		due := now.Add(time.Duration(postpaidTerms(acc, c).TermsDays) * 24 * time.Hour)
		doc.DueAt = &due
	}
	var vatTx *domain.Transaction
	if vat > 0 {
		// La TVA collectée sort du revenu vers son compte.
		vatTx, err = domain.NewTransaction(org, domain.TxVAT, domain.VATReference(doc.ID), c.Currency, period, "billing",
			map[string]domain.Milli{domain.AccountRevenue: -vat, domain.AccountVAT: vat}, now)
		if err != nil {
			return err
		}
		vatTx.Period = period
	}
	issued, err := s.Phase4.IssueDocument(ctx, doc, s.Issuer.InvoicePrefix, func(number string) ([]byte, error) {
		return pdf.Render(pdf.Doc{Kind: domain.DocInvoice, Number: number, Period: period, Currency: c.Currency, IssuedAt: now,
			Data: data, Total: cash, VAT: vat, VATRateBP: s.Issuer.VATRateBP, DueAt: doc.DueAt})
	}, vatTx)
	if issued {
		s.log().Infow("billing: invoice issued", "organization", org, "period", period, "number", doc.Number)
	}
	return err
}

// issueReceipts : un reçu par recharge réussie.
func (s *Service) issueReceipts(ctx context.Context) {
	if s.Phase4 == nil || !s.Issuer.Configured() {
		return
	}
	topups, err := s.Phase4.TopupsWithoutReceipt(ctx, 100)
	if err != nil {
		s.log().Warnw("billing: receipts lookup failed", "error", err)
		return
	}
	for _, t := range topups {
		if err := s.issueReceipt(ctx, t); err != nil {
			s.log().Warnw("billing: receipt not issued", "topup", t.ID, "error", err)
		}
	}
}

func (s *Service) issueReceipt(ctx context.Context, t domain.Topup) error {
	acc, err := s.Ledger.Account(ctx, t.OrganizationID)
	if err != nil {
		return err
	}
	issuedAt := s.now()
	if t.SettledAt != nil {
		issuedAt = *t.SettledAt
	}
	payment := operatorLabel(t.Operator) + " " + domain.MaskPhone(t.Phone)
	data := domain.DocData{Issuer: s.Issuer, Customer: acc.Profile, OrgName: s.orgName(ctx, t.OrganizationID), Payment: payment,
		Lines: []domain.DocLine{{Label: "Recharge du solde prépayé", Amount: t.Amount}}}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	tid := t.ID
	doc := &domain.Document{ID: uuid.New(), OrganizationID: t.OrganizationID, Kind: domain.DocReceipt, TopupID: &tid,
		Currency: t.Currency, Total: t.Amount, Data: raw, IssuedAt: issuedAt}
	_, err = s.Phase4.IssueDocument(ctx, doc, s.Issuer.ReceiptPrefix, func(number string) ([]byte, error) {
		return pdf.Render(pdf.Doc{Kind: domain.DocReceipt, Number: number, Currency: t.Currency, IssuedAt: issuedAt, Data: data, Total: t.Amount})
	}, nil)
	return err
}

func operatorLabel(op string) string {
	switch op {
	case "cg_mtn":
		return "MTN Mobile Money"
	case "cg_airtel":
		return "Airtel Money"
	default:
		return op
	}
}

// ── Crédit d'essai : une fois par numéro Mobile Money ───────────────────

// grantTrialOnTopup : à la première recharge réussie d'une organisation, le
// crédit d'essai de la grille, si ce numéro n'en a jamais donné un. Le
// paiement prouve que le numéro appartient au client (pas de SMS à vérifier).
func (s *Service) grantTrialOnTopup(ctx context.Context, t *domain.Topup) {
	if s.Phase4 == nil || t == nil || t.Status != domain.TopupSucceeded {
		return
	}
	c, err := s.catalog(ctx, t.OrganizationID)
	if err != nil || c.TrialCredit <= 0 {
		return
	}
	claimed, err := s.Phase4.ClaimTrial(ctx, t.Phone, t.OrganizationID, s.now())
	if err != nil || !claimed {
		return
	}
	now := s.now()
	g := &domain.CreditGrant{ID: uuid.New(), OrganizationID: t.OrganizationID, Amount: c.TrialCredit, Currency: c.Currency,
		ExpiresAt: now.Add(time.Duration(c.TrialDays) * 24 * time.Hour), Reason: "trial"}
	if _, err := s.Ledger.GrantCredit(ctx, g, domain.TrialReference(t.OrganizationID), "billing", now); err != nil {
		s.log().Warnw("billing: trial credit not granted", "organization", t.OrganizationID, "error", err)
	}
}

// afterTopup : recharge réglée ; crédit d'essai, puis fin de grâce ou reprise.
func (s *Service) afterTopup(ctx context.Context, t *domain.Topup) {
	if t == nil || t.Status != domain.TopupSucceeded {
		return
	}
	s.grantTrialOnTopup(ctx, t)
	if err := s.settleAccount(ctx, t.OrganizationID); err != nil {
		s.log().Warnw("billing: account not settled after top-up", "organization", t.OrganizationID, "error", err)
	}
	s.checkBalance(ctx, t.OrganizationID)
}
