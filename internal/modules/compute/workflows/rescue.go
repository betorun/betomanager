package workflows

import (
	"context"
	"encoding/json"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

// Attente de l'e-mail du fournisseur (identifiants du mode secours).
const (
	rescueMailTimeout = 15 * time.Minute
	rescueMailPoll    = 30 * time.Second
)

// KindMailbox : diagnostic de la boîte technique (demande de l'équipe).
const KindMailbox = "compute.mailbox"

// enterRescue : démarrage sur le système de secours, puis interception de
// l'e-mail du fournisseur ; jamais arrivé : le client et l'équipe le voient.
func enterRescue(ctx workflow.Context, requestID string) error {
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
	if err := workflow.ExecuteActivity(quick(ctx), a.RescueBoot, spec.ProviderID, true).Get(ctx, &task); err != nil {
		return err
	}
	if err := waitTask(ctx, spec.ProviderID, task); err != nil {
		return err
	}
	if err := workflow.ExecuteActivity(quick(ctx), a.MarkRescue, spec.VPSID, domain.RescueActive, domain.RescueSecretWaiting).Get(ctx, nil); err != nil {
		return err
	}
	deadline := workflow.Now(ctx).Add(rescueMailTimeout)
	for {
		var found bool
		if err := workflow.ExecuteActivity(quick(ctx), a.CaptureRescueSecret, spec.VPSID).Get(ctx, &found); err != nil {
			return err
		}
		if found {
			return nil
		}
		if workflow.Now(ctx).After(deadline) {
			return workflow.ExecuteActivity(quick(ctx), a.RescueMailMissing, spec.VPSID).Get(ctx, nil)
		}
		if err := workflow.Sleep(ctx, rescueMailPoll); err != nil {
			return err
		}
	}
}

// exitRescue : retour au disque, secret effacé.
func exitRescue(ctx workflow.Context, requestID string) error {
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
	if err := workflow.ExecuteActivity(quick(ctx), a.RescueBoot, spec.ProviderID, false).Get(ctx, &task); err != nil {
		return err
	}
	if err := waitTask(ctx, spec.ProviderID, task); err != nil {
		return err
	}
	if err := workflow.ExecuteActivity(quick(ctx), a.MarkRescue, spec.VPSID, "", "").Get(ctx, nil); err != nil {
		return err
	}
	return workflow.ExecuteActivity(quick(ctx), a.Observe, spec.VPSID).Get(ctx, nil)
}

// ── Activités ───────────────────────────────────────────────────────────

// RescueBoot : démarrage choisi (secours ou disque), puis redémarrage.
func (a *Activities) RescueBoot(ctx context.Context, providerID string, on bool) (string, error) {
	p, err := a.provider()
	if err != nil {
		return "", err
	}
	if a.Rescue == nil {
		return "", nonRetryable("no compute provider configured", "NoProvider", nil)
	}
	if err := a.Rescue.SetRescue(ctx, providerID, on); err != nil {
		return "", err
	}
	return p.Reboot(ctx, providerID)
}

// MarkRescue : état du secours ; en sortie, le mot de passe est effacé.
func (a *Activities) MarkRescue(ctx context.Context, vpsID, rescue, secret string) error {
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return err
	}
	if rescue == "" {
		if err := a.Store.DeleteSecrets(ctx, v.ID, domain.SecretRescuePassword); err != nil {
			return err
		}
	}
	if err := a.Store.UpdateVPS(ctx, v.ID, map[string]any{"rescue": rescue, "rescue_secret": secret, "updated_at": a.now()}); err != nil {
		return err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	return nil
}

// CaptureRescueSecret : cherche l'e-mail du fournisseur, chiffre les
// identifiants, efface l'e-mail. Le mot de passe ne sort jamais de cette
// activité (ni résultat, ni journal, ni historique Temporal).
func (a *Activities) CaptureRescueSecret(ctx context.Context, vpsID string) (bool, error) {
	if a.Mailbox == nil || a.Sealer == nil {
		return false, nil // sans boîte : le délai finira en « e-mail manquant »
	}
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return false, err
	}
	since := v.UpdatedAt
	if v.RescueRequestedAt != nil {
		since = *v.RescueRequestedAt
	}
	mail, err := a.Mailbox.FindRescue(ctx, v.ProviderID, since)
	if err != nil || mail == nil {
		return false, err
	}
	plain, err := json.Marshal(mail.Credentials)
	if err != nil {
		return false, err
	}
	sealed, err := a.Sealer.Seal(plain, service.RescueAAD(v.ID))
	if err != nil {
		return false, err
	}
	now := a.now()
	if err := a.Store.SaveSecret(ctx, &domain.Secret{VPSID: v.ID, Kind: domain.SecretRescuePassword, Ciphertext: sealed,
		CreatedAt: now, ExpiresAt: now.Add(domain.RescueSecretTTL)}); err != nil {
		return false, err
	}
	if err := a.Mailbox.Delete(ctx, mail.ID); err != nil {
		activity.GetLogger(ctx).Warn("compute: rescue email not deleted", "vps", v.ID.String(), "error", err.Error())
	}
	if err := a.Store.UpdateVPS(ctx, v.ID, map[string]any{"rescue_secret": domain.RescueSecretReady, "updated_at": now}); err != nil {
		return false, err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	return true, nil
}

// RescueMailMissing : alerte (journal d'erreur, page VPS de l'équipe, fiche du client).
func (a *Activities) RescueMailMissing(ctx context.Context, vpsID string) error {
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return err
	}
	activity.GetLogger(ctx).Error("compute: rescue email not received", "vps", v.ID.String(), "provider_id", v.ProviderID,
		"waited", rescueMailTimeout.String())
	if err := a.Store.UpdateVPS(ctx, v.ID, map[string]any{"rescue_secret": domain.RescueSecretMissing, "updated_at": a.now()}); err != nil {
		return err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	return nil
}

// CheckMailbox : la boîte technique répond (diagnostic de l'équipe).
func (a *Activities) CheckMailbox(ctx context.Context) error {
	if a.Mailbox == nil {
		return nonRetryable("no technical mailbox configured", "NoMailbox", nil)
	}
	_, err := a.Mailbox.Check(ctx)
	if err != nil {
		return nonRetryable(err.Error(), "Mailbox", err)
	}
	return nil
}
