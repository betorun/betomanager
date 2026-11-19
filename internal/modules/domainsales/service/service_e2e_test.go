package service_test

// Revente de domaines sur un vrai Postgres, avec un faux registraire et la
// vraie facturation. Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/domainsales/... -count=1 -v

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/domains/registrar"
	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	billingInventory "github.com/thekrauss/kubemanager/internal/modules/billing/inventory"
	billingRepo "github.com/thekrauss/kubemanager/internal/modules/billing/repository"
	billingSvc "github.com/thekrauss/kubemanager/internal/modules/billing/service"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales/domain"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales/repository"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales/service"
	workloadsDomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

const e2eDSN = "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"

// fakeRegistrar : OVH simulé.
type fakeRegistrar struct {
	price, renew int64
	taken        bool
	status       string
	orders       int
	autoRenew    map[string]bool
	records      []registrar.Record
	expiration   time.Time
}

func (f *fakeRegistrar) Check(_ context.Context, d string) (*registrar.Availability, error) {
	return &registrar.Availability{Domain: d, Available: !f.taken, PriceCents: f.price, RenewCents: f.renew, Currency: "EUR"}, nil
}
func (f *fakeRegistrar) Register(context.Context, registrar.Order) (*registrar.OrderResult, error) {
	f.orders++
	return &registrar.OrderResult{OrderID: "9001", PriceCents: f.price}, nil
}
func (f *fakeRegistrar) OrderStatus(context.Context, string) (string, error) { return f.status, nil }
func (f *fakeRegistrar) Info(context.Context, string) (*registrar.DomainInfo, error) {
	return &registrar.DomainInfo{Expiration: f.expiration, AutoRenew: true}, nil
}
func (f *fakeRegistrar) SetAutoRenew(_ context.Context, d string, on bool) error {
	f.autoRenew[d] = on
	return nil
}
func (f *fakeRegistrar) SetRecords(_ context.Context, _ string, r []registrar.Record) error {
	f.records = append(f.records, r...)
	return nil
}

// fakeWorkloads : conteneurs de l'organisation.
type fakeWorkloads struct{ org uuid.UUID }

func (f fakeWorkloads) WorkloadOrg(context.Context, uuid.UUID) (uuid.UUID, error) { return f.org, nil }
func (f fakeWorkloads) AddDomain(_ context.Context, _ uuid.UUID, host, _ string) (*workloadsDomain.DomainResponse, error) {
	return &workloadsDomain.DomainResponse{WorkloadDomain: workloadsDomain.WorkloadDomain{ID: uuid.New(), Hostname: host}, Records: []workloadsDomain.DNSRecord{
		{Type: "TXT", Name: "_betorun-challenge." + host, Value: "tok"}, {Type: "A", Name: host, Value: "203.0.113.10"},
		{Type: "ALIAS", Name: host, Value: "edge.betorun.app", Alternative: true}}}, nil
}
func (f fakeWorkloads) VerifyDomain(context.Context, uuid.UUID, uuid.UUID) (*workloadsDomain.DomainResponse, error) {
	return &workloadsDomain.DomainResponse{Pointing: true}, nil
}

func TestE2E_DomainSales(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	org := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e domains')`, org).Error)
	now := time.Date(2030, 4, 2, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	billing := &billingSvc.Service{Store: billingRepo.New(db), Inventory: billingInventory.New(db), Ledger: billingRepo.NewLedger(db),
		Admin: billingRepo.NewAdmin(db), Phase4: billingRepo.NewPhase4(db), Postpaid: billingRepo.NewPostpaid(db), Now: clock}
	reg := &fakeRegistrar{price: 849, renew: 849, status: registrar.OrderPending, autoRenew: map[string]bool{},
		expiration: time.Date(2031, 4, 2, 0, 0, 0, 0, time.UTC)}
	s := &service.Service{Store: repository.New(db), Registrar: reg, Wallet: billing, Workloads: fakeWorkloads{org: org}, Ordering: true, Now: clock}
	name := "e2e-" + uuid.NewString()[:8] + ".com"
	owner := domain.Contact{FirstName: "Awa", LastName: "Diop", Email: "awa@ex.sn", Phone: "+242 06 123 45 67", Address: "1 rue", City: "Brazzaville", Country: "cg"}

	q, err := s.Search(ctx, org, "WWW."+name)
	require.NoError(t, err)
	assert.True(t, q.Available)
	assert.Equal(t, int64(7000), q.Price, "8,49 € → 7 000 FCFA (marge et arrondi de la grille)")

	// Sans solde : refusé, rien n'est commandé.
	_, err = s.Buy(ctx, org, domain.BuyRequest{Domain: name, Price: q.Price, Owner: owner, AutoRenew: true}, "user:client")
	assert.ErrorIs(t, err, billingDomain.ErrInsufficientFunds)

	// Solde suffisant : débité, commande passée en tâche de fond.
	_, err = billing.Adjust(ctx, org, billingDomain.AdjustmentRequest{Amount: 10000, Reason: "test"}, "user:staff")
	require.NoError(t, err)
	_, err = s.Buy(ctx, org, domain.BuyRequest{Domain: name, Price: 6500, Owner: owner}, "user:client")
	assert.ErrorIs(t, err, domain.ErrPriceChanged, "prix annoncé obsolète")
	v, err := s.Buy(ctx, org, domain.BuyRequest{Domain: name, Price: q.Price, Owner: owner, AutoRenew: true}, "user:client")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusOrdering, v.Status)
	cash, _, err := billingRepo.NewLedger(db).Balances(ctx, org)
	require.NoError(t, err)
	assert.Equal(t, billingDomain.FromUnits(3000), cash)
	q2, err := s.Search(ctx, org, name)
	require.NoError(t, err)
	assert.False(t, q2.Available, "déjà en commande")

	s.Process(ctx)
	assert.Equal(t, 1, reg.orders)
	s.Process(ctx)
	assert.Equal(t, 1, reg.orders, "jamais commandé deux fois")
	reg.status = registrar.OrderDelivered
	s.Process(ctx)
	list, err := s.List(ctx, org)
	require.NoError(t, err)
	require.Len(t, list.Domains, 1)
	assert.Equal(t, domain.StatusActive, list.Domains[0].Status)
	assert.True(t, reg.autoRenew[name])

	// Branché sur un conteneur : DNS écrits (sans l'alternative ALIAS), racine et www.
	att, err := s.Attach(ctx, org, uuid.MustParse(v.ID), domain.AttachRequest{WorkloadID: uuid.NewString(), WWW: true})
	require.NoError(t, err)
	assert.True(t, att.Verified)
	assert.Equal(t, []string{name, "www." + name}, att.Hostnames)
	assert.Len(t, reg.records, 4, "TXT et A, pour la racine et www")

	// Renouvellement 30 jours avant : solde insuffisant → le registraire ne renouvellera pas.
	now = reg.expiration.Add(-20 * 24 * time.Hour)
	s.Process(ctx)
	assert.False(t, reg.autoRenew[name])
	list, _ = s.List(ctx, org)
	assert.True(t, list.Domains[0].RenewFailed)
	// Recharge puis réactivation : débité, renouvellement rétabli.
	_, err = billing.Adjust(ctx, org, billingDomain.AdjustmentRequest{Amount: 10000, Reason: "test"}, "user:staff")
	require.NoError(t, err)
	_, err = s.SetAutoRenew(ctx, org, uuid.MustParse(v.ID), true)
	require.NoError(t, err)
	s.Process(ctx)
	assert.True(t, reg.autoRenew[name])
	cash, _, _ = billingRepo.NewLedger(db).Balances(ctx, org)
	assert.Equal(t, billingDomain.FromUnits(6000), cash, "3 000 + 10 000 − 7 000 de renouvellement")
	s.Process(ctx)
	cash2, _, _ := billingRepo.NewLedger(db).Balances(ctx, org)
	assert.Equal(t, cash, cash2, "un seul débit par période")

	// Échec chez le registraire : remboursé.
	other := "e2e-" + uuid.NewString()[:8] + ".com"
	_, err = billing.Adjust(ctx, org, billingDomain.AdjustmentRequest{Amount: 10000, Reason: "test"}, "user:staff")
	require.NoError(t, err)
	cash, _, _ = billingRepo.NewLedger(db).Balances(ctx, org)
	_, err = s.Buy(ctx, org, domain.BuyRequest{Domain: other, Price: 7000, Owner: owner}, "user:client")
	require.NoError(t, err)
	reg.status = registrar.OrderFailed
	s.Process(ctx) // commande passée
	s.Process(ctx) // échec constaté
	cash3, _, _ := billingRepo.NewLedger(db).Balances(ctx, org)
	assert.Equal(t, cash, cash3, "remboursé")
}
