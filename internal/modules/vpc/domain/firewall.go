package domain

import "net/netip"

// ValidateRules : règles du client, avant toute mise en œuvre. max : règles
// du client permises (FirewallCapabilities.MaxRules). Une règle réservée
// n'est jamais acceptée du client.
func ValidateRules(rules []FirewallRule, max int, ipv6 bool) error {
	if len(rules) > max {
		return ErrTooManyRules
	}
	for _, r := range rules {
		if err := validRule(r, ipv6); err != nil {
			return err
		}
	}
	return nil
}

func validRule(r FirewallRule, ipv6 bool) error {
	if r.Reserved {
		return ErrInvalidRule
	}
	if r.Action != ActionAllow && r.Action != ActionDeny {
		return ErrInvalidRule
	}
	switch r.Protocol {
	case ProtocolTCP, ProtocolUDP:
		if r.PortFrom < 0 || r.PortTo < 0 || r.PortFrom > 65535 || r.PortTo > 65535 {
			return ErrInvalidRule
		}
		if r.PortTo != 0 && (r.PortFrom == 0 || r.PortTo < r.PortFrom) {
			return ErrInvalidRule
		}
	case ProtocolICMP, ProtocolAll:
		if r.PortFrom != 0 || r.PortTo != 0 {
			return ErrInvalidRule // pas de port hors TCP et UDP
		}
	default:
		return ErrInvalidRule
	}
	if r.Source == "" {
		return nil
	}
	p, err := netip.ParsePrefix(r.Source)
	if err != nil {
		a, aerr := netip.ParseAddr(r.Source)
		if aerr != nil {
			return ErrInvalidRule
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if p.Addr().Is6() && !ipv6 {
		return ErrInvalidRule
	}
	if p != p.Masked() {
		return ErrInvalidRule // bloc mal aligné (10.0.0.1/8)
	}
	return nil
}
