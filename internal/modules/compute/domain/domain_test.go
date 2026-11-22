package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMonthlyPrice(t *testing.T) {
	// 6,49 € TTC × 655,957 × 2 = 8 514,32 FCFA → 9 000 (palier de 500 supérieur).
	o := Offer{ProviderPriceCents: 649, CoefficientBP: 20000, RoundTo: 500}
	assert.Equal(t, int64(9000), o.MonthlyPrice(655957))
	o.RoundTo = 1
	assert.Equal(t, int64(8515), o.MonthlyPrice(655957), "sans palier : au FCFA supérieur")
	assert.Zero(t, Offer{CoefficientBP: 20000, RoundTo: 500}.MonthlyPrice(655957), "non tarifée : invendable")
	assert.Zero(t, o.MonthlyPrice(0), "taux absent : invendable")
	// Prix exactement sur un palier : il ne monte pas.
	assert.Equal(t, int64(1000), Offer{ProviderPriceCents: 100, CoefficientBP: 10000, RoundTo: 500}.MonthlyPrice(1000000))
}

func TestSells(t *testing.T) {
	o := Offer{Locations: json.RawMessage(`["gra","rbx"]`), Images: json.RawMessage(`{"ubuntu-24.04":"Ubuntu 24.04","debian-12":"Debian 12"}`)}
	assert.True(t, o.Sells(LocationGRA, ImageUbuntu))
	assert.False(t, o.Sells(LocationSBG, ImageUbuntu))
	assert.False(t, o.Sells(LocationGRA, "windows-2022"))
	assert.Empty(t, Offer{Locations: json.RawMessage(`{bad`)}.LocationList())
}
