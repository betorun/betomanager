package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

// Délais de la création (temps du fournisseur, pas de l'activité).
const (
	deliveryTimeout = 6 * time.Hour
	readyTimeout    = 2 * time.Hour
	taskTimeout     = time.Hour
	deliveryPoll    = time.Minute
	// deliveryPollMax : le sondage de la livraison s'espace (×1,5) jusqu'à ce
	// plafond (une livraison lente ne fait pas des centaines d'appels).
	deliveryPollMax = 10 * time.Minute
	readyPoll       = 30 * time.Second
	taskPoll        = 15 * time.Second
)

// Types d'erreur non rejouables (raison de l'échec du VPS).
const (
	errTypePriceAbove = "PriceAbove"
	errTypeProvider   = "Provider"
	errTypeTimeout    = "Timeout"
)

// VMSpec : ce que le workflow sait d'une demande sur un VPS.
type VMSpec struct {
	VPSID        string
	State        string
	ProviderID   string
	OrderID      string
	SSHPublicKey string
	Period       string
	AmountMilli  int64
	// Réinstallation : système demandé et empreinte de la nouvelle clé.
	Image       string
	Fingerprint string
	// Sauvegardes : point et mode de restauration, description du snapshot.
	RestorePoint time.Time
	Mode         string
	Description  string
}

func quick(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 5 * time.Second, BackoffCoefficient: 2, MaximumAttempts: 8},
	})
}

// once : jamais rejoué (commande payée chez le fournisseur).
func once(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
}

// createVM : commande → livraison → installation avec la clé du client → en
// marche. Tout échec rembourse le mois et marque le VPS en échec.
func createVM(ctx workflow.Context, requestID string) error {
	var a *Activities
	var spec VMSpec
	if err := workflow.ExecuteActivity(quick(ctx), a.LoadVM, requestID).Get(ctx, &spec); err != nil {
		return err
	}
	err := runCreate(ctx, &spec)
	if err == nil {
		return nil
	}
	code := domain.FailureProvider
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		switch appErr.Type() {
		case errTypePriceAbove:
			code = domain.FailurePriceChanged
		case errTypeTimeout:
			code = domain.FailureTimeout
		}
	}
	if ferr := workflow.ExecuteActivity(quick(ctx), a.FailCreate, spec, code).Get(ctx, nil); ferr != nil {
		return ferr
	}
	return err
}

func runCreate(ctx workflow.Context, spec *VMSpec) error {
	var a *Activities
	if spec.ProviderID == "" {
		if spec.OrderID == "" {
			if err := workflow.ExecuteActivity(once(ctx), a.PlaceOrder, spec.VPSID).Get(ctx, &spec.OrderID); err != nil {
				return err
			}
		}
		deadline := workflow.Now(ctx).Add(deliveryTimeout)
		poll := deliveryPoll
		for {
			var st OrderStatus
			if err := workflow.ExecuteActivity(quick(ctx), a.OrderState, spec.OrderID).Get(ctx, &st); err != nil {
				return err
			}
			if st.State == service.OrderFailed {
				return temporal.NewNonRetryableApplicationError("provider order failed", errTypeProvider, nil)
			}
			if st.State == service.OrderDelivered {
				pid := st.ProviderID
				if pid == "" {
					// Le détail de la commande ne nomme pas le VPS : rattaché par
					// l'équipe, ou seul VPS inconnu du compte.
					if err := workflow.ExecuteActivity(quick(ctx), a.ResolveDelivered, spec.VPSID).Get(ctx, &pid); err != nil {
						return err
					}
				}
				if pid != "" {
					spec.ProviderID = pid
					break
				}
			}
			if workflow.Now(ctx).After(deadline) {
				return temporal.NewNonRetryableApplicationError("provider delivery timed out", errTypeTimeout, nil)
			}
			if err := workflow.Sleep(ctx, poll); err != nil {
				return err
			}
			poll = min(poll*3/2, deliveryPollMax)
		}
		if err := workflow.ExecuteActivity(quick(ctx), a.AttachProvider, spec.VPSID, spec.ProviderID).Get(ctx, nil); err != nil {
			return err
		}
	}
	if err := waitReady(ctx, spec.ProviderID); err != nil {
		return err
	}
	var task string
	if err := workflow.ExecuteActivity(quick(ctx), a.Rebuild, spec.VPSID, spec.ProviderID, spec.SSHPublicKey).Get(ctx, &task); err != nil {
		return err
	}
	if err := waitTask(ctx, spec.ProviderID, task); err != nil {
		return err
	}
	return workflow.ExecuteActivity(quick(ctx), a.MarkObserved, spec.VPSID, domain.StateRunning).Get(ctx, nil)
}

func waitReady(ctx workflow.Context, providerID string) error {
	var a *Activities
	deadline := workflow.Now(ctx).Add(readyTimeout)
	for {
		var ready bool
		if err := workflow.ExecuteActivity(quick(ctx), a.Ready, providerID).Get(ctx, &ready); err != nil {
			return err
		}
		if ready {
			return nil
		}
		if workflow.Now(ctx).After(deadline) {
			return temporal.NewNonRetryableApplicationError("VPS not ready in time", errTypeTimeout, nil)
		}
		if err := workflow.Sleep(ctx, readyPoll); err != nil {
			return err
		}
	}
}

func waitTask(ctx workflow.Context, providerID, task string) error {
	if task == "" || task == "0" {
		return nil
	}
	var a *Activities
	deadline := workflow.Now(ctx).Add(taskTimeout)
	for {
		var state string
		if err := workflow.ExecuteActivity(quick(ctx), a.TaskState, providerID, task).Get(ctx, &state); err != nil {
			return err
		}
		switch state {
		case service.TaskDone:
			return nil
		case service.TaskError, service.TaskCancelled:
			return temporal.NewNonRetryableApplicationError("provider task "+state, errTypeProvider, nil)
		}
		if workflow.Now(ctx).After(deadline) {
			return temporal.NewNonRetryableApplicationError("provider task timed out", errTypeTimeout, nil)
		}
		if err := workflow.Sleep(ctx, taskPoll); err != nil {
			return err
		}
	}
}

// powerVM : démarrer ou arrêter (suspension, relance après paiement).
func powerVM(ctx workflow.Context, requestID string, start bool) error {
	var a *Activities
	var spec VMSpec
	if err := workflow.ExecuteActivity(quick(ctx), a.LoadVM, requestID).Get(ctx, &spec); err != nil {
		return err
	}
	if spec.ProviderID == "" {
		return nil // jamais livré : rien à allumer ni éteindre
	}
	if err := waitReady(ctx, spec.ProviderID); err != nil {
		return err
	}
	var task string
	if err := workflow.ExecuteActivity(quick(ctx), a.Power, spec.ProviderID, start).Get(ctx, &task); err != nil {
		return err
	}
	if err := waitTask(ctx, spec.ProviderID, task); err != nil {
		return err
	}
	state := domain.StateStopped
	if start {
		state = domain.StateRunning
	}
	return workflow.ExecuteActivity(quick(ctx), a.MarkObserved, spec.VPSID, state).Get(ctx, nil)
}

// rebootVM : redémarrage (le VPS reste en marche).
func rebootVM(ctx workflow.Context, requestID string) error {
	var a *Activities
	var spec VMSpec
	if err := workflow.ExecuteActivity(quick(ctx), a.LoadVM, requestID).Get(ctx, &spec); err != nil {
		return err
	}
	if spec.ProviderID == "" {
		return nil
	}
	if err := waitReady(ctx, spec.ProviderID); err != nil {
		return err
	}
	var task string
	if err := workflow.ExecuteActivity(quick(ctx), a.Reboot, spec.ProviderID).Get(ctx, &task); err != nil {
		return err
	}
	if err := waitTask(ctx, spec.ProviderID, task); err != nil {
		return err
	}
	return workflow.ExecuteActivity(quick(ctx), a.MarkObserved, spec.VPSID, domain.StateRunning).Get(ctx, nil)
}

// reinstallVM : nouveau système avec la clé donnée ; le disque est effacé.
func reinstallVM(ctx workflow.Context, requestID string) error {
	var a *Activities
	var spec VMSpec
	if err := workflow.ExecuteActivity(quick(ctx), a.LoadVM, requestID).Get(ctx, &spec); err != nil {
		return err
	}
	if spec.ProviderID == "" {
		return nil
	}
	if err := waitReady(ctx, spec.ProviderID); err != nil {
		return err
	}
	var task string
	if err := workflow.ExecuteActivity(quick(ctx), a.Reinstall, spec.VPSID, spec.ProviderID, spec.Image, spec.SSHPublicKey).Get(ctx, &task); err != nil {
		return err
	}
	if err := waitTask(ctx, spec.ProviderID, task); err != nil {
		return err
	}
	if err := workflow.ExecuteActivity(quick(ctx), a.SetImage, spec.VPSID, spec.Image, spec.Fingerprint).Get(ctx, nil); err != nil {
		return err
	}
	return workflow.ExecuteActivity(quick(ctx), a.MarkObserved, spec.VPSID, domain.StateRunning).Get(ctx, nil)
}

// ── Activités ───────────────────────────────────────────────────────────

func nonRetryable(msg, typ string, err error) error {
	return temporal.NewNonRetryableApplicationError(msg, typ, err)
}

func (a *Activities) provider() (service.ComputeProvider, error) {
	if a.Provider == nil {
		return nil, nonRetryable("no compute provider configured", "NoProvider", nil)
	}
	return a.Provider, nil
}

// LoadVM : la demande et son VPS.
func (a *Activities) LoadVM(ctx context.Context, requestID string) (*VMSpec, error) {
	id, err := uuid.Parse(requestID)
	if err != nil {
		return nil, nonRetryable("invalid request id", "InvalidRequest", err)
	}
	r, err := a.Store.Request(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil || r.VPSID == nil {
		return nil, nonRetryable("compute request without VPS", "RequestNotFound", nil)
	}
	v, err := a.Store.VPS(ctx, *r.VPSID)
	if err != nil {
		if errors.Is(err, domain.ErrVPSNotFound) {
			return nil, nonRetryable(err.Error(), "VPSNotFound", err)
		}
		return nil, err
	}
	spec := &VMSpec{VPSID: v.ID.String(), State: v.State, ProviderID: v.ProviderID, OrderID: v.ProviderOrderID}
	if r.Kind == domain.KindCreate {
		var p domain.CreatePayload
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			return nil, nonRetryable("invalid create payload", "InvalidRequest", err)
		}
		spec.SSHPublicKey, spec.Period, spec.AmountMilli = p.SSHPublicKey, p.Period, p.AmountMilli
	}
	switch r.Kind {
	case domain.KindRestore, domain.KindUnmount:
		var p domain.RestorePayload
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			return nil, nonRetryable("invalid restore payload", "InvalidRequest", err)
		}
		spec.RestorePoint, spec.Mode = p.RestorePoint, p.Mode
	case domain.KindSnapshotCreate:
		var p domain.SnapshotPayload
		_ = json.Unmarshal(r.Payload, &p)
		spec.Description = p.Description
	case domain.KindSnapshotOrder:
		var p domain.SubscribePayload
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			return nil, nonRetryable("invalid subscribe payload", "InvalidRequest", err)
		}
		spec.Period, spec.AmountMilli = p.Period, p.AmountMilli
	}
	if r.Kind == domain.KindReinstall {
		var p domain.ReinstallPayload
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			return nil, nonRetryable("invalid reinstall payload", "InvalidRequest", err)
		}
		spec.Image, spec.SSHPublicKey, spec.Fingerprint = p.Image, p.SSHPublicKey, p.Fingerprint
	}
	return spec, nil
}

func parseUUID(s string) (uuid.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, nonRetryable("invalid VPS id", "InvalidRequest", err)
	}
	return u, nil
}

func (a *Activities) vps(ctx context.Context, id string) (*domain.VPS, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, nonRetryable("invalid VPS id", "InvalidRequest", err)
	}
	return a.Store.VPS(ctx, u)
}

// PlaceOrder : commande chez le fournisseur (une seule fois : l'identifiant
// est gardé ; jamais rejouée par Temporal).
func (a *Activities) PlaceOrder(ctx context.Context, vpsID string) (string, error) {
	if a.Orderer == nil {
		return "", nonRetryable("no compute provider configured", "NoProvider", nil)
	}
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return "", err
	}
	if v.ProviderOrderID != "" {
		return v.ProviderOrderID, nil
	}
	if v.State != domain.StatePending && v.State != domain.StateCreating {
		// Résilié ou en échec entre-temps : rien n'est commandé.
		return "", nonRetryable("VPS no longer awaiting creation", errTypeProvider, nil)
	}
	o, err := a.Store.Offer(ctx, v.Offer)
	if err != nil {
		return "", err
	}
	if err := a.Store.UpdateVPS(ctx, v.ID, map[string]any{"state": domain.StateCreating, "updated_at": a.now()}); err != nil {
		return "", err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	orderID, err := a.Orderer.Order(ctx, service.OrderSpec{Reference: v.ID.String(), PlanCode: o.ProviderPlanCode,
		Location: v.Location, Image: o.ImageMap()[v.Image], MaxPriceCents: o.ProviderPriceCents})
	if err != nil {
		if errors.Is(err, service.ErrProviderPriceAbove) {
			return "", nonRetryable(err.Error(), errTypePriceAbove, err)
		}
		return "", nonRetryable(err.Error(), errTypeProvider, err)
	}
	return orderID, a.Store.UpdateVPS(ctx, v.ID, map[string]any{"provider_order_id": orderID, "updated_at": a.now()})
}

// OrderStatus : suivi de la commande.
type OrderStatus struct {
	State      string
	ProviderID string
}

func (a *Activities) OrderState(ctx context.Context, orderID string) (*OrderStatus, error) {
	if a.Orderer == nil {
		return nil, nonRetryable("no compute provider configured", "NoProvider", nil)
	}
	st, pid, err := a.Orderer.OrderState(ctx, orderID)
	if err != nil {
		return nil, err
	}
	return &OrderStatus{State: st, ProviderID: pid}, nil
}

// AttachProvider : le VPS livré est rattaché à son identifiant chez le fournisseur.
func (a *Activities) AttachProvider(ctx context.Context, vpsID, providerID string) error {
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return err
	}
	fields := map[string]any{"provider_id": providerID, "updated_at": a.now()}
	if v.State == domain.StatePending || v.State == domain.StateCreating {
		fields["state"] = domain.StateCreating // un VPS résilié le reste
	}
	if err := a.Store.UpdateVPS(ctx, v.ID, fields); err != nil {
		return err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	return nil
}

// Ready : le VPS existe chez le fournisseur et n'a pas d'opération en cours.
func (a *Activities) Ready(ctx context.Context, providerID string) (bool, error) {
	p, err := a.provider()
	if err != nil {
		return false, err
	}
	vm, err := p.Get(ctx, providerID)
	if err != nil {
		return false, err
	}
	return vm != nil && !vm.Busy, nil
}

// Reboot : redémarrage chez le fournisseur.
func (a *Activities) Reboot(ctx context.Context, providerID string) (string, error) {
	p, err := a.provider()
	if err != nil {
		return "", err
	}
	return p.Reboot(ctx, providerID)
}

// Reinstall : système choisi par le client (image de la plateforme), sa clé.
func (a *Activities) Reinstall(ctx context.Context, vpsID, providerID, image, sshKey string) (string, error) {
	p, err := a.provider()
	if err != nil {
		return "", err
	}
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return "", err
	}
	o, err := a.Store.Offer(ctx, v.Offer)
	if err != nil {
		return "", err
	}
	name := o.ImageMap()[image]
	if name == "" {
		return "", nonRetryable("image not sold with this offer", errTypeProvider, nil)
	}
	task, err := p.Rebuild(ctx, providerID, name, sshKey, installScript(v))
	if err != nil {
		return "", nonRetryable(err.Error(), errTypeProvider, err)
	}
	return task, nil
}

// SetImage : système et clé installés.
func (a *Activities) SetImage(ctx context.Context, vpsID, image, fingerprint string) error {
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return err
	}
	if err := a.Store.UpdateVPS(ctx, v.ID, map[string]any{"image": image, "ssh_key_fingerprint": fingerprint, "updated_at": a.now()}); err != nil {
		return err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	return nil
}

// Rebuild : installation du système avec la clé du client.
func (a *Activities) Rebuild(ctx context.Context, vpsID, providerID, sshKey string) (string, error) {
	p, err := a.provider()
	if err != nil {
		return "", err
	}
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return "", err
	}
	o, err := a.Store.Offer(ctx, v.Offer)
	if err != nil {
		return "", err
	}
	task, err := p.Rebuild(ctx, providerID, o.ImageMap()[v.Image], sshKey, installScript(v))
	if err != nil {
		return "", nonRetryable(err.Error(), errTypeProvider, err)
	}
	return task, nil
}

func (a *Activities) TaskState(ctx context.Context, providerID, taskID string) (string, error) {
	p, err := a.provider()
	if err != nil {
		return "", err
	}
	t, err := p.Task(ctx, providerID, taskID)
	if err != nil {
		return "", err
	}
	return t.State, nil
}

// Power : démarrer ou arrêter ; déjà dans l'état voulu : aucune opération.
func (a *Activities) Power(ctx context.Context, providerID string, start bool) (string, error) {
	p, err := a.provider()
	if err != nil {
		return "", err
	}
	vm, err := p.Get(ctx, providerID)
	if err != nil {
		return "", err
	}
	if vm == nil {
		return "", nonRetryable("VPS not found at provider", errTypeProvider, nil)
	}
	if start && vm.State == domain.StateRunning || !start && vm.State == domain.StateStopped {
		return "", nil
	}
	if start {
		return p.Start(ctx, providerID)
	}
	return p.Stop(ctx, providerID)
}

// MarkObserved : état et adresses relevés chez le fournisseur. Un VPS
// suspendu reste suspendu à l'arrêt ; relancé après paiement, il repart.
func (a *Activities) MarkObserved(ctx context.Context, vpsID, state string) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return err
	}
	fields := map[string]any{"updated_at": a.now()}
	switch {
	case v.State == domain.StateDeleted || v.State == domain.StateFailed:
		// résilié ou en échec : jamais remis en marche par une opération
	case state == domain.StateStopped && v.State == domain.StateSuspended:
	case state == domain.StateRunning && v.State == domain.StateSuspended && v.FailureCode != domain.FailureUnpaid:
		// suspendu par l'équipe entre-temps : on ne le rouvre pas
	default:
		fields["state"] = state
		if state == domain.StateRunning {
			fields["failure_code"] = ""
		}
	}
	if vm, err := p.Get(ctx, v.ProviderID); err == nil && vm != nil {
		if vm.IPv4 != "" {
			fields["ipv4"] = vm.IPv4
		}
		if vm.IPv6 != "" {
			fields["ipv6"] = vm.IPv6
		}
	}
	if err := a.Store.UpdateVPS(ctx, v.ID, fields); err != nil {
		return err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	return nil
}

// ResolveDelivered : identifiant du VPS livré quand la commande ne le donne
// pas : celui rattaché par l'équipe (back-office), sinon le seul VPS du compte
// inconnu de la plateforme ; plusieurs candidats : attendre l'équipe.
func (a *Activities) ResolveDelivered(ctx context.Context, vpsID string) (string, error) {
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return "", err
	}
	if v.ProviderID != "" {
		return v.ProviderID, nil
	}
	p, err := a.provider()
	if err != nil {
		return "", err
	}
	known, err := a.Store.ProviderVPS(ctx, p.Name())
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, k := range known {
		taken[k.ProviderID] = true
	}
	// Un VPS déjà relevé avant la commande n'est pas celui-ci (orphelin plus ancien).
	machines, err := a.Store.Machines(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range machines {
		if m.FirstSeenAt != nil && m.FirstSeenAt.Before(v.CreatedAt) {
			taken[m.ProviderID] = true
		}
	}
	o, err := a.Store.Offer(ctx, v.Offer)
	if err != nil {
		return "", err
	}
	vms, err := untakenVMs(ctx, p, taken)
	if err != nil {
		return "", err
	}
	candidate := ""
	for _, vm := range vms {
		if taken[vm.ProviderID] || !MatchesOrder(vm, o.ProviderPlanCode, v.Location) {
			continue
		}
		if candidate != "" {
			return "", nil // plusieurs VPS inconnus : l'équipe tranche
		}
		candidate = vm.ProviderID
	}
	return candidate, nil
}

// nameLister : fournisseur qui liste les noms sans lire chaque VPS.
type nameLister interface {
	Names(ctx context.Context) ([]string, error)
}

// untakenVMs : VPS du compte non rattachés ; seuls ceux-là sont lus quand le
// fournisseur sait lister les noms (pas une lecture du parc à chaque sondage).
func untakenVMs(ctx context.Context, p service.ComputeProvider, taken map[string]bool) ([]service.ProviderVM, error) {
	nl, ok := p.(nameLister)
	if !ok {
		return p.List(ctx)
	}
	names, err := nl.Names(ctx)
	if err != nil {
		return nil, err
	}
	var out []service.ProviderVM
	for _, n := range names {
		if taken[n] {
			continue
		}
		vm, err := p.Get(ctx, n)
		if err != nil {
			return nil, err
		}
		if vm != nil {
			out = append(out, *vm)
		}
	}
	return out, nil
}

// MatchesOrder : le VPS du fournisseur correspond au plan et à l'emplacement
// commandés (sans ces informations : non, l'équipe tranche). Deux commandes
// simultanées ne s'échangent jamais leurs serveurs.
func MatchesOrder(vm service.ProviderVM, planCode, location string) bool {
	plan, want := strings.ToLower(strings.TrimSpace(vm.Plan)), strings.ToLower(strings.TrimSpace(planCode))
	if plan == "" || want == "" || location == "" {
		return false
	}
	if plan != want && !strings.HasSuffix(want, plan) && !strings.HasSuffix(plan, want) {
		return false
	}
	return strings.Contains(strings.ToLower(vm.Zone), strings.ToLower(location))
}

// installScript : port 25 sortant fermé, sauf ouverture par l'équipe.
func installScript(v *domain.VPS) string {
	if v.SMTPAllowed {
		return ""
	}
	return domain.SMTPBlockScript
}

// FailCreate : VPS en échec, mois remboursé (idempotent par référence).
func (a *Activities) FailCreate(ctx context.Context, spec VMSpec, code string) error {
	v, err := a.vps(ctx, spec.VPSID)
	if err != nil {
		return err
	}
	if a.Wallet != nil && spec.AmountMilli > 0 && spec.Period != "" {
		if err := a.Wallet.RefundCompute(ctx, v.OrganizationID, billingDomain.Milli(spec.AmountMilli), spec.Period+":refund",
			"VPS "+v.Name+" non livré, remboursé"); err != nil {
			return err
		}
	}
	if err := a.Store.UpdateVPS(ctx, v.ID, map[string]any{"state": domain.StateFailed, "failure_code": code,
		"paid_until": nil, "updated_at": a.now()}); err != nil {
		return err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	return nil
}
