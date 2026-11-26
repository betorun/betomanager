package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateRules(t *testing.T) {
	ssh := FirewallRule{Action: ActionAllow, Protocol: ProtocolTCP, PortFrom: 22}
	web := FirewallRule{Action: ActionAllow, Protocol: ProtocolTCP, PortFrom: 80, PortTo: 443, Source: "0.0.0.0/0"}
	deny := FirewallRule{Action: ActionDeny, Protocol: ProtocolAll}
	assert.NoError(t, ValidateRules([]FirewallRule{ssh, web, deny}, 16, false))
	assert.NoError(t, ValidateRules([]FirewallRule{{Action: ActionAllow, Protocol: ProtocolICMP, Source: "203.0.113.7"}}, 16, false))
	assert.ErrorIs(t, ValidateRules([]FirewallRule{ssh, web}, 1, false), ErrTooManyRules)

	bad := []FirewallRule{
		{Action: "accept", Protocol: ProtocolTCP},
		{Action: ActionAllow, Protocol: "sctp"},
		{Action: ActionAllow, Protocol: ProtocolICMP, PortFrom: 22},
		{Action: ActionAllow, Protocol: ProtocolTCP, PortFrom: 443, PortTo: 80},
		{Action: ActionAllow, Protocol: ProtocolTCP, PortTo: 80},
		{Action: ActionAllow, Protocol: ProtocolTCP, PortFrom: 70000},
		{Action: ActionAllow, Protocol: ProtocolTCP, Source: "pas-une-ip"},
		{Action: ActionAllow, Protocol: ProtocolTCP, Source: "10.0.0.1/8"},
		{Action: ActionAllow, Protocol: ProtocolTCP, Source: "2001:db8::/32"},
		{Action: ActionAllow, Protocol: ProtocolTCP, Reserved: true},
	}
	for _, r := range bad {
		assert.ErrorIs(t, ValidateRules([]FirewallRule{r}, 16, false), ErrInvalidRule, "%+v", r)
	}
	assert.NoError(t, ValidateRules([]FirewallRule{{Action: ActionAllow, Protocol: ProtocolTCP, Source: "2001:db8::/32"}}, 16, true))
}
