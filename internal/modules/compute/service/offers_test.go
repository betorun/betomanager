package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

type offerStore struct {
	Store
	offers []domain.Offer
}

func (s offerStore) Offers(context.Context) ([]domain.Offer, error) { return s.offers, nil }

type twoRates struct{}

func (twoRates) Rate(context.Context, uuid.UUID) (domain.Rate, error) {
	return domain.Rate{Currency: "XAF", EURMilli: 655957, RoundTo: 500}, nil
}
func (twoRates) Rates(context.Context) ([]domain.Rate, error) {
	return []domain.Rate{{Currency: "XAF", EURMilli: 655957, RoundTo: 500}, {Currency: "EUR", EURMilli: 1000, RoundTo: 1}}, nil
}

func TestAdminOffersPrices(t *testing.T) {
	s := &Service{Catalogs: twoRates{}, Store: offerStore{offers: []domain.Offer{
		{Code: "VPS-1", ProviderPlanCode: "vps-2025-model1", ProviderPriceCents: 649, CoefficientBP: 20000, RoundTo: 500,
			SnapshotPlanCode: "option-snapshot", SnapshotPriceCents: 120, Public: true,
			Locations: json.RawMessage(`["gra"]`), Images: json.RawMessage(`{"ubuntu-24.04":"Ubuntu 24.04"}`)},
		{Code: "VPS-2", CoefficientBP: 20000, RoundTo: 500, Locations: json.RawMessage(`[]`), Images: json.RawMessage(`{}`)},
	}}}
	list, err := s.AdminOffers(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, []OfferPrice{{"XAF", 9000, 2000}, {"EUR", 13, 3}}, list[0].Prices,
		"palier de chaque grille : 500 FCFA, 1 € (jamais 500 €)")
	assert.True(t, list[0].Sellable)
	assert.False(t, list[1].Sellable, "ni plan ni prix")
}
