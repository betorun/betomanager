package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// Alertes de solde de la facturation (ADR 0012) : sept jours, deux jours,
// solde épuisé. Envoyées aux owners de l'organisation, une fois par seuil
// (le module billing retient le dernier seuil prévenu).

const (
	// KindBalance : une alerte de solde par organisation et par seuil.
	KindBalance = "billing.balance"
	// BalanceEvery : garde-fou si le seuil était réarmé trop souvent.
	BalanceEvery = 12 * time.Hour
	pageBilling  = "/billing"
)

var orgOutro = map[i18n.Locale]string{
	i18n.FR: "Vous recevez cet e-mail parce que vous êtes propriétaire de cette organisation. Une alerte par seuil de solde.",
	i18n.EN: "You receive this email because you own this organization. One alert per balance threshold.",
}

var balanceCopies = map[string]map[i18n.Locale]alertCopy{
	"low": pair(
		alertCopy{"Solde bas pour « {{.Project}} »", "Votre solde baisse",
			"Au rythme actuel, le solde de l'organisation « {{.Project}} » ({{.Name}}) couvre moins d'une semaine. Rechargez par mobile money depuis la console pour éviter toute interruption.",
			"Recharger"},
		alertCopy{"Low balance for \"{{.Project}}\"", "Your balance is getting low",
			"At the current rate, the balance of organization \"{{.Project}}\" ({{.Name}}) covers less than a week. Top up by mobile money from the console to avoid any interruption.",
			"Top up"}),
	"critical": pair(
		alertCopy{"Solde presque épuisé pour « {{.Project}} »", "Votre solde est presque épuisé",
			"Au rythme actuel, le solde de l'organisation « {{.Project}} » ({{.Name}}) couvre moins de deux jours. Rechargez dès que possible.",
			"Recharger"},
		alertCopy{"Balance almost used up for \"{{.Project}}\"", "Your balance is almost used up",
			"At the current rate, the balance of organization \"{{.Project}}\" ({{.Name}}) covers less than two days. Top up as soon as possible.",
			"Top up"}),
	"empty": pair(
		alertCopy{"Solde épuisé pour « {{.Project}} »", "Votre solde est épuisé",
			"Le solde de l'organisation « {{.Project}} » est épuisé ({{.Name}}). Vos ressources continuent de tourner pendant le délai de grâce ; rechargez pour éviter leur arrêt.",
			"Recharger"},
		alertCopy{"Balance used up for \"{{.Project}}\"", "Your balance is used up",
			"The balance of organization \"{{.Project}}\" is used up ({{.Name}}). Your resources keep running during the grace period; top up to avoid them being stopped.",
			"Top up"}),
	"suspended": pair(
		alertCopy{"Conteneurs arrêtés dans « {{.Project}} »", "Vos conteneurs sont arrêtés",
			"Le solde de l'organisation « {{.Project}} » est resté épuisé après le délai de grâce ({{.Name}}) : ses conteneurs ont été arrêtés. Leurs données sont conservées pour un temps limité : vous serez prévenu avant toute suppression. Rechargez : ils redémarreront tout seuls.",
			"Recharger"},
		alertCopy{"Containers stopped in \"{{.Project}}\"", "Your containers are stopped",
			"The balance of organization \"{{.Project}}\" stayed used up after the grace period ({{.Name}}): its containers were stopped. Their data is kept for a limited time: you will be warned before anything is deleted. Top up: they will restart by themselves.",
			"Top up"}),
}

// BalanceAlert : solde passé sous un seuil (status : low, critical, empty).
func (n *Notifier) BalanceAlert(ctx context.Context, org uuid.UUID, status string, balance int64, _ *int64, currency string) {
	if n == nil {
		return
	}
	copies, ok := balanceCopies[status]
	if !ok {
		return
	}
	n.run(ctx, func(ctx context.Context) {
		if n.Mail == nil || n.Store == nil {
			return
		}
		// Sujet : l'organisation et le seuil (un seuil, une alerte au plus).
		subject := uuid.NewSHA1(org, []byte(status))
		claimed, err := n.Store.Claim(ctx, subject, KindBalance, BalanceEvery)
		if err != nil || !claimed {
			if err != nil {
				n.log().Warnw("balance alert throttle check failed", "organization", org, zap.Error(err))
			}
			return
		}
		recipients, err := n.Store.OrgRecipients(ctx, org)
		if err != nil {
			n.log().Warnw("balance alert recipients lookup failed", "organization", org, zap.Error(err))
			return
		}
		name, err := n.Store.OrgName(ctx, org)
		if err != nil {
			n.log().Warnw("balance alert organization lookup failed", "organization", org, zap.Error(err))
			return
		}
		amount := money(balance, currency)
		link := strings.TrimRight(n.ConsoleURL, "/") + pageBilling
		for _, r := range recipients {
			msg, err := renderWith(ctx, copies, r, name, amount, link, orgOutro)
			if err == nil {
				err = n.Mail.Send(ctx, msg)
			}
			if err != nil {
				n.log().Warnw("balance alert email failed", "organization", org, zap.Error(err))
			}
		}
	})
}

// money : "12 500 FCFA" (XAF et XOF en FCFA).
func money(amount int64, currency string) string {
	neg := amount < 0
	if neg {
		amount = -amount
	}
	s := fmt.Sprintf("%d", amount)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
	}
	unit := strings.ToUpper(currency)
	if unit == "XAF" || unit == "XOF" {
		unit = "FCFA"
	}
	out := b.String() + " " + unit
	if neg {
		out = "-" + out
	}
	return out
}

// ── Budgets par projet ──────────────────────────────────────────────────

// KindBudget : une alerte par projet et par seuil (80 %, 100 %).
const KindBudget = "billing.budget"

var budgetCopies = map[string]map[i18n.Locale]alertCopy{
	"near": pair(
		alertCopy{"80 % du budget de « {{.Project}} »", "Budget bientôt atteint",
			"Le projet « {{.Project}} » a consommé {{.Name}} de son budget mensuel.", "Voir la facturation"},
		alertCopy{"80% of the budget of \"{{.Project}}\"", "Budget almost reached",
			"Project \"{{.Project}}\" has used {{.Name}} of its monthly budget.", "View billing"}),
	"over": pair(
		alertCopy{"Budget atteint pour « {{.Project}} »", "Budget mensuel atteint",
			"Le projet « {{.Project}} » a atteint son budget mensuel ({{.Name}}).", "Voir la facturation"},
		alertCopy{"Budget reached for \"{{.Project}}\"", "Monthly budget reached",
			"Project \"{{.Project}}\" reached its monthly budget ({{.Name}}).", "View billing"}),
	"stopped": pair(
		alertCopy{"Budget atteint : conteneurs arrêtés dans « {{.Project}} »", "Budget atteint, conteneurs arrêtés",
			"Le projet « {{.Project}} » a atteint son budget mensuel ({{.Name}}) : ses conteneurs ont été arrêtés, comme demandé. Relevez le budget pour les relancer, ou ils redémarreront le mois prochain.",
			"Voir la facturation"},
		alertCopy{"Budget reached: containers stopped in \"{{.Project}}\"", "Budget reached, containers stopped",
			"Project \"{{.Project}}\" reached its monthly budget ({{.Name}}): its containers were stopped, as requested. Raise the budget to restart them, or they will restart next month.",
			"View billing"}),
}

// BudgetAlert : budget d'un projet franchi (level 1 : 80 %, 2 : 100 %).
func (n *Notifier) BudgetAlert(ctx context.Context, project uuid.UUID, level int, spent, limit int64, currency string, stopped bool) {
	if n == nil {
		return
	}
	key := "near"
	if level >= 2 {
		key = "over"
		if stopped {
			key = "stopped"
		}
	}
	amount := money(spent, currency) + " / " + money(limit, currency)
	subject := uuid.NewSHA1(project, []byte(fmt.Sprintf("budget-%d", level)))
	n.run(ctx, func(ctx context.Context) {
		n.deliver(ctx, project, subject, KindBudget, BalanceEvery, budgetCopies[key], amount, pageBilling)
	})
}

// ── Postpayé : facture en retard ────────────────────────────────────────

// KindInvoiceOverdue : une relance par facture (le module billing la retient aussi).
const KindInvoiceOverdue = "billing.invoice_overdue"

var invoiceOverdueCopies = pair(
	alertCopy{"Facture {{.Name}} en retard", "Une facture attend son paiement",
		"La facture {{.Name}} de l'organisation « {{.Project}} » n'est pas encore réglée. Réglez-la par virement ou par mobile money depuis la console : sans paiement, les conteneurs de l'organisation seront arrêtés.",
		"Voir la facture"},
	alertCopy{"Invoice {{.Name}} overdue", "An invoice is waiting for payment",
		"Invoice {{.Name}} of organization \"{{.Project}}\" isn't paid yet. Pay it by bank transfer or by mobile money from the console: without payment, the organization's containers will be stopped.",
		"View the invoice"})

// InvoiceOverdue : facture postpayée impayée après son échéance.
func (n *Notifier) InvoiceOverdue(ctx context.Context, org uuid.UUID, number string, unpaid int64, dueAt time.Time, currency string) {
	if n == nil {
		return
	}
	label := fmt.Sprintf("%s (%s)", number, money(unpaid, currency))
	n.run(ctx, func(ctx context.Context) {
		if n.Mail == nil || n.Store == nil {
			return
		}
		subject := uuid.NewSHA1(org, []byte("invoice-"+number))
		if claimed, err := n.Store.Claim(ctx, subject, KindInvoiceOverdue, BalanceEvery); err != nil || !claimed {
			return
		}
		recipients, err := n.Store.OrgRecipients(ctx, org)
		if err != nil {
			return
		}
		name, err := n.Store.OrgName(ctx, org)
		if err != nil {
			return
		}
		link := strings.TrimRight(n.ConsoleURL, "/") + pageBilling
		for _, r := range recipients {
			msg, err := renderWith(ctx, invoiceOverdueCopies, r, name, label, link, orgOutro)
			if err == nil {
				err = n.Mail.Send(ctx, msg)
			}
			if err != nil {
				n.log().Warnw("invoice reminder email failed", "organization", org, zap.Error(err))
			}
		}
	})
}

// ── Suppression des données après une longue suspension ────────────────

// KindPurge : un e-mail par organisation et par étape (le module billing la retient aussi).
const KindPurge = "billing.purge"

// Étapes (billing/domain) : 1 à 3 avertissements, 4 suppression faite.
var purgeCopies = map[int]map[i18n.Locale]alertCopy{
	1: pair(
		alertCopy{"« {{.Project}} » : vos données seront supprimées le {{.Name}}", "Vos données seront supprimées",
			"L'organisation « {{.Project}} » est suspendue faute de paiement. Sans paiement d'ici le {{.Name}}, ses conteneurs et leurs disques, ses bases de données et ses sites seront supprimés. Rechargez dès maintenant : tout redémarre tout seul.",
			"Recharger"},
		alertCopy{"\"{{.Project}}\": your data will be deleted on {{.Name}}", "Your data will be deleted",
			"Organization \"{{.Project}}\" is suspended for lack of payment. Without payment by {{.Name}}, its containers and their disks, its databases and its sites will be deleted. Top up now: everything restarts by itself.",
			"Top up"}),
	2: pair(
		alertCopy{"Une semaine avant la suppression des données de « {{.Project}} »", "Plus qu'une semaine",
			"Les conteneurs et leurs disques, les bases de données et les sites de l'organisation « {{.Project}} » seront supprimés le {{.Name}}. Rechargez pour les garder : tout redémarre tout seul.",
			"Recharger"},
		alertCopy{"One week before the data of \"{{.Project}}\" is deleted", "One week left",
			"The containers and their disks, the databases and the sites of organization \"{{.Project}}\" will be deleted on {{.Name}}. Top up to keep them: everything restarts by itself.",
			"Top up"}),
	3: pair(
		alertCopy{"Dernier avertissement : données de « {{.Project}} » supprimées le {{.Name}}", "Dernier avertissement",
			"Sans paiement, les conteneurs et leurs disques, les bases de données et les sites de l'organisation « {{.Project}} » seront supprimés le {{.Name}}. Cette suppression est définitive.",
			"Recharger"},
		alertCopy{"Last warning: data of \"{{.Project}}\" deleted on {{.Name}}", "Last warning",
			"Without payment, the containers and their disks, the databases and the sites of organization \"{{.Project}}\" will be deleted on {{.Name}}. This deletion is permanent.",
			"Top up"}),
	4: pair(
		alertCopy{"Données de « {{.Project}} » supprimées", "Vos données ont été supprimées",
			"L'organisation « {{.Project}} » est restée suspendue : ses conteneurs, ses bases de données et ses sites ont été supprimés le {{.Name}}. L'organisation, ses projets et ses factures restent ; une copie de chaque base est gardée quelque temps : écrivez au support pour la récupérer.",
			"Voir la facturation"},
		alertCopy{"Data of \"{{.Project}}\" deleted", "Your data was deleted",
			"Organization \"{{.Project}}\" stayed suspended: its containers, databases and sites were deleted on {{.Name}}. The organization, its projects and its invoices remain; a copy of each database is kept for a while: write to support to get it back.",
			"View billing"}),
}

// PurgeNotice : avertissement avant la suppression des données (stage 1 à
// 3), ou suppression faite (4). purgeAt : date de la suppression.
func (n *Notifier) PurgeNotice(ctx context.Context, org uuid.UUID, stage int, purgeAt time.Time) {
	if n == nil {
		return
	}
	copies, ok := purgeCopies[stage]
	if !ok {
		return
	}
	date := purgeAt.UTC().Format("2006-01-02")
	n.run(ctx, func(ctx context.Context) {
		if n.Mail == nil || n.Store == nil {
			return
		}
		subject := uuid.NewSHA1(org, []byte(fmt.Sprintf("purge-%d-%s", stage, date)))
		if claimed, err := n.Store.Claim(ctx, subject, KindPurge, BalanceEvery); err != nil || !claimed {
			return
		}
		recipients, err := n.Store.OrgRecipients(ctx, org)
		if err != nil {
			n.log().Warnw("purge notice recipients lookup failed", "organization", org, zap.Error(err))
			return
		}
		name, err := n.Store.OrgName(ctx, org)
		if err != nil {
			return
		}
		link := strings.TrimRight(n.ConsoleURL, "/") + pageBilling
		for _, r := range recipients {
			msg, err := renderWith(ctx, copies, r, name, date, link, orgOutro)
			if err == nil {
				err = n.Mail.Send(ctx, msg)
			}
			if err != nil {
				n.log().Warnw("purge notice email failed", "organization", org, zap.Error(err))
			}
		}
	})
}
