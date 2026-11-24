package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/thekrauss/kubemanager/internal/operator/action"
)

// Actions sur les nœuds : jouées par node-agent, avec un ordre signé qui
// porte la validation de la carte.

const kindServiceRestart = "node.service.restart"

// critical : services dont le redémarrage coupe le nœud entier (plan de
// contrôle, moteur de conteneurs) : risque élevé, nom retapé.
var critical = map[string]bool{"k3s": true, "k3s-agent": true, "containerd": true, "docker": true, "kubelet": true}

type serviceParams struct {
	Node    string `json:"node"`
	Service string `json:"service"`
}

// Actions : action.Provider. Aucune action sans clé d'ordres ni nœud.
func (n *Nodes) Actions() []action.Spec {
	if n == nil || len(n.names) == 0 || n.order == nil {
		return nil
	}
	return []action.Spec{{
		Kind: kindServiceRestart,
		Description: "Redémarrer un service de la liste fermée d'un serveur (unité systemd ou conteneur Docker de la plateforme). " +
			"Seulement les services marqués redémarrables par node_status.",
		Parameters: schema(pNode+`,"service":{"type":"string","description":"Nom du service (node_status)"}`, "node", "service"),
		Capability: action.CapInfrastructure,
	}}
}

// Prepare : action.Provider. Le service doit exister sur le nœud et être
// redémarrable ; le risque dépend du service.
func (n *Nodes) Prepare(ctx context.Context, kind string, raw json.RawMessage) (action.Prepared, error) {
	if kind != kindServiceRestart {
		return action.Prepared{}, action.ErrUnknown
	}
	var p serviceParams
	if err := decode(raw, &p); err != nil {
		return action.Prepared{}, err
	}
	if _, ok := n.urls[p.Node]; !ok {
		return action.Prepared{}, fmt.Errorf("%w : serveur inconnu %q ; serveurs : %s", action.ErrRefused, p.Node, strings.Join(n.names, ", "))
	}
	st, err := n.status(ctx, p.Node)
	if err != nil {
		return action.Prepared{}, fmt.Errorf("%s injoignable : %v", p.Node, err)
	}
	var svc *service
	var names []string
	for i := range st.Services {
		names = append(names, st.Services[i].Name)
		if st.Services[i].Name == p.Service {
			svc = &st.Services[i]
		}
	}
	if svc == nil {
		sort.Strings(names)
		return action.Prepared{}, fmt.Errorf("%w : service %q absent de la liste de %s (%s)", action.ErrRefused, p.Service, p.Node, strings.Join(names, ", "))
	}
	if !svc.Restartable {
		return action.Prepared{}, fmt.Errorf("%w : %s n'est pas redémarrable sur %s (configuration du nœud)", action.ErrRefused, p.Service, p.Node)
	}
	params, _ := json.Marshal(p)
	out := action.Prepared{
		Target:  p.Node + " / " + p.Service,
		Summary: fmt.Sprintf("Redémarrer le service %s (%s) sur le serveur %s.", p.Service, svc.Kind, p.Node),
		Params:  params,
		Risk:    action.RiskMedium,
		State:   serviceLine(*svc),
	}
	if critical[p.Service] {
		out.Risk, out.Confirm = action.RiskHigh, p.Node
		out.Summary += " Tout le serveur est touché pendant le redémarrage."
	}
	return out, nil
}

// Execute : action.Provider : ordre signé, puis l'état relu.
func (n *Nodes) Execute(ctx context.Context, kind string, raw json.RawMessage, a action.Approval) (string, error) {
	if kind != kindServiceRestart {
		return "", action.ErrUnknown
	}
	var p serviceParams
	if err := decode(raw, &p); err != nil {
		return "", err
	}
	if err := n.Restart(ctx, p.Node, p.Service, Approval{ApprovedBy: a.ApprovedBy, ApprovalID: a.CardID, InvestigationID: a.InvestigationID}); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Redémarrage de %s demandé à %s.", p.Service, p.Node)
	if st, err := n.status(ctx, p.Node); err == nil {
		for _, s := range st.Services {
			if s.Name == p.Service {
				msg += " État relu : " + serviceLine(s) + "."
				break
			}
		}
	}
	return msg, nil
}

func serviceLine(s service) string {
	keys := make([]string, 0, len(s.Detail))
	for k := range s.Detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{s.State}
	for _, k := range keys {
		parts = append(parts, k+"="+s.Detail[k])
	}
	return strings.Join(parts, " ")
}
