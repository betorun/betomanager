// Package country : codes de pays ISO 3166-1 alpha-2 (pays du client, porté
// par son organisation ; table pays → région du back-office).
package country

import (
	"errors"
	"strings"
)

// ErrInvalid : code qui n'est pas celui d'un pays ISO 3166-1 alpha-2.
var ErrInvalid = errors.New("invalid country: two-letter ISO 3166-1 code (e.g. GA)")

// Normalize : code en majuscules, sans espaces autour.
func Normalize(v string) string { return strings.ToUpper(strings.TrimSpace(v)) }

// Valid : code ISO 3166-1 alpha-2 attribué (majuscules).
func Valid(code string) bool {
	_, ok := codes[code]
	return ok
}

// Parse : code normalisé ; "" reste "" (pays inconnu) ; sinon ErrInvalid
// s'il n'est pas attribué.
func Parse(v string) (string, error) {
	c := Normalize(v)
	if c == "" || Valid(c) {
		return c, nil
	}
	return "", ErrInvalid
}

// codes : les 249 codes officiellement attribués (ISO 3166-1 alpha-2).
var codes = func() map[string]struct{} {
	const all = "AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ " +
		"BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ " +
		"CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ " +
		"DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR " +
		"GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY " +
		"HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP " +
		"KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY " +
		"MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ " +
		"NA NC NE NF NG NI NL NO NP NR NU NZ OM " +
		"PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW " +
		"SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ " +
		"TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ " +
		"VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW"
	m := map[string]struct{}{}
	for _, c := range strings.Fields(all) {
		m[c] = struct{}{}
	}
	return m
}()
