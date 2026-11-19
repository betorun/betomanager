// Package pdf : factures et reçus en PDF (go-pdf/fpdf, Go pur). Le PDF est
// rendu une fois, à l'émission, puis archivé tel quel : il ne se régénère
// jamais (un document émis ne change plus).
package pdf

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// Doc : ce qu'il faut pour rendre un document.
type Doc struct {
	Kind     string // domain.DocInvoice ou domain.DocReceipt
	Number   string
	Period   string
	Currency string
	IssuedAt time.Time
	Data     domain.DocData
	// Total TTC, TVA et taux (facture), en millièmes.
	Total     domain.Milli
	VAT       domain.Milli
	VATRateBP int
	// DueAt : échéance (postpayé) ; nil : déjà réglée par le solde prépayé.
	DueAt *time.Time
}

// Couleurs (marque betorun, sobres à l'impression).
var (
	ink    = [3]int{33, 25, 28}
	muted  = [3]int{110, 100, 104}
	accent = [3]int{122, 31, 61}
	line   = [3]int{220, 214, 216}
)

// money : "12 500 FCFA" (XAF et XOF en FCFA), arrondi à l'unité.
func money(m domain.Milli, currency string) string {
	v := m.Units()
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d", v)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
	}
	unit := strings.ToUpper(currency)
	if unit == "XAF" || unit == "XOF" {
		unit = "FCFA"
	}
	out := b.String() + " " + unit
	if neg {
		out = "- " + out
	}
	return out
}

var months = []string{"janvier", "février", "mars", "avril", "mai", "juin", "juillet", "août", "septembre", "octobre", "novembre", "décembre"}

// periodLabel : "2026-11" → "novembre 2026".
func periodLabel(p string) string {
	t, err := time.Parse("2006-01", p)
	if err != nil {
		return p
	}
	return months[t.Month()-1] + " " + fmt.Sprint(t.Year())
}

func dateFR(t time.Time) string {
	t = t.UTC()
	day := fmt.Sprint(t.Day())
	if t.Day() == 1 {
		day = "1er"
	}
	return fmt.Sprintf("%s %s %d", day, months[t.Month()-1], t.Year())
}

// Render : le document en PDF (A4, français).
func Render(d Doc) ([]byte, error) {
	p := fpdf.New("P", "mm", "A4", "")
	tr := p.UnicodeTranslatorFromDescriptor("") // UTF-8 → cp1252 (accents, espaces)
	p.SetTitle(tr(d.Number), false)
	p.SetAuthor(tr(d.Data.Issuer.Name), false)
	p.SetMargins(18, 18, 18)
	p.SetAutoPageBreak(true, 20)
	p.AddPage()

	color := func(c [3]int) { p.SetTextColor(c[0], c[1], c[2]) }
	text := func(h float64, s string) { p.MultiCell(0, h, tr(s), "", "L", false) }

	// En-tête : émetteur à gauche, nature et numéro à droite.
	p.SetFont("Helvetica", "B", 16)
	color(accent)
	p.CellFormat(100, 8, tr(d.Data.Issuer.Name), "", 0, "L", false, 0, "")
	title := "Facture"
	if d.Kind == domain.DocReceipt {
		title = "Reçu"
	}
	p.CellFormat(0, 8, tr(title), "", 1, "R", false, 0, "")
	p.SetFont("Helvetica", "", 9.5)
	color(muted)
	issuer := []string{d.Data.Issuer.Address}
	if d.Data.Issuer.TaxID != "" {
		issuer = append(issuer, "N° fiscal : "+d.Data.Issuer.TaxID)
	}
	y := p.GetY()
	p.MultiCell(100, 4.6, tr(strings.Join(issuer, "\n")), "", "L", false)
	after := p.GetY()
	p.SetXY(118, y)
	meta := []string{"N° " + d.Number, "Émis le " + dateFR(d.IssuedAt)}
	if d.Period != "" {
		meta = append(meta, "Période : "+periodLabel(d.Period))
	}
	if d.DueAt != nil {
		meta = append(meta, "À payer avant le "+dateFR(*d.DueAt))
	}
	p.MultiCell(0, 4.6, tr(strings.Join(meta, "\n")), "", "R", false)
	p.SetY(max(after, p.GetY()) + 8)

	// Client.
	p.SetFont("Helvetica", "B", 10)
	color(ink)
	text(5, "Client")
	p.SetFont("Helvetica", "", 10)
	cust := d.Data.Customer
	name := cust.LegalName
	if name == "" {
		name = d.Data.OrgName
	}
	lines := []string{name}
	if cust.Address != "" {
		lines = append(lines, cust.Address)
	}
	if cust.Country != "" {
		lines = append(lines, cust.Country)
	}
	if cust.TaxID != "" {
		lines = append(lines, "N° fiscal : "+cust.TaxID)
	}
	if cust.LegalName != "" && d.Data.OrgName != "" && d.Data.OrgName != cust.LegalName {
		lines = append(lines, "Organisation betorun : "+d.Data.OrgName)
	}
	p.MultiCell(0, 5, tr(strings.Join(lines, "\n")), "", "L", false)
	p.Ln(6)

	// Lignes.
	p.SetDrawColor(line[0], line[1], line[2])
	p.SetFont("Helvetica", "B", 9.5)
	color(muted)
	p.CellFormat(130, 7, tr("Désignation"), "B", 0, "L", false, 0, "")
	p.CellFormat(0, 7, tr("Montant TTC"), "B", 1, "R", false, 0, "")
	for _, l := range d.Data.Lines {
		p.SetFont("Helvetica", "", 10)
		color(ink)
		label := l.Label
		if l.Detail != "" {
			label += " — " + l.Detail
		}
		p.CellFormat(130, 7, tr(label), "B", 0, "L", false, 0, "")
		p.CellFormat(0, 7, tr(money(l.Amount, d.Currency)), "B", 1, "R", false, 0, "")
	}
	p.Ln(3)

	row := func(label, value string, bold bool) {
		style := ""
		if bold {
			style = "B"
		}
		p.SetFont("Helvetica", style, 10)
		p.CellFormat(130, 6.5, tr(label), "", 0, "R", false, 0, "")
		p.CellFormat(0, 6.5, tr(value), "", 1, "R", false, 0, "")
	}
	color(ink)
	if d.Kind == domain.DocInvoice {
		var consumption domain.Milli
		for _, l := range d.Data.Lines {
			consumption += l.Amount
		}
		row("Total de la consommation", money(consumption, d.Currency), false)
		if d.Data.PaidByCredits > 0 {
			row("Réglé par crédits offerts", money(-d.Data.PaidByCredits, d.Currency), false)
		}
		if d.DueAt != nil {
			row("Net à payer TTC", money(d.Total, d.Currency), true)
		} else {
			row("Net à payer TTC (réglé par le solde)", money(d.Total, d.Currency), true)
		}
		if d.VATRateBP > 0 {
			ht := d.Total - d.VAT
			row("dont montant HT", money(ht, d.Currency), false)
			row(fmt.Sprintf("dont TVA %s %%", rate(d.VATRateBP)), money(d.VAT, d.Currency), false)
		}
	} else {
		row("Montant reçu", money(d.Total, d.Currency), true)
		if d.Data.Payment != "" {
			row("Moyen de paiement", d.Data.Payment, false)
		}
	}

	// Pied : mention sur le prépayé.
	p.Ln(10)
	p.SetFont("Helvetica", "", 8.5)
	color(muted)
	if d.Kind == domain.DocInvoice && d.DueAt != nil {
		text(4.2, "Facture de la consommation du mois, à régler avant le "+dateFR(*d.DueAt)+" par virement ou par mobile money depuis la console betorun. Prix TTC.")
		if d.Data.Issuer.PaymentInstructions != "" {
			p.Ln(2)
			text(4.2, d.Data.Issuer.PaymentInstructions)
		}
	} else if d.Kind == domain.DocInvoice {
		text(4.2, "Facture de la consommation du mois, déjà réglée par le solde prépayé du compte (aucun paiement à effectuer). Prix TTC.")
	} else {
		text(4.2, "Reçu d'une recharge du solde prépayé. Le solde est consommé selon l'usage ; la TVA figure sur les factures mensuelles de consommation.")
	}

	var buf bytes.Buffer
	if err := p.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// rate : 1890 → "18,90".
func rate(bp int) string {
	return strings.Replace(fmt.Sprintf("%.2f", float64(bp)/100), ".", ",", 1)
}
