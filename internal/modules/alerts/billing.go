package alerts

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
			"Le solde de l'organisation « {{.Project}} » est resté épuisé après le délai de grâce ({{.Name}}) : ses conteneurs ont été arrêtés. Leurs données sont conservées. Rechargez : ils redémarreront tout seuls.",
			"Recharger"},
		alertCopy{"Containers stopped in \"{{.Project}}\"", "Your containers are stopped",
			"The balance of organization \"{{.Project}}\" stayed used up after the grace period ({{.Name}}): its containers were stopped. Their data is kept. Top up: they will restart by themselves.",
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
