// Package llmsetup : assemble la pile llm depuis la configuration.
//
//	Instrument( Chain( Breaker(Budget(primary)), Breaker(Budget(fallback)) ) )
//
// Le budget est sous le disjoncteur : un refus de budget n'est pas une panne
// du fournisseur et ne doit pas l'ouvrir (Kind non compté).
package llmsetup

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/redis/go-redis/v9"

	"github.com/thekrauss/kubemanager/internal/core/configs"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/anthropic"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/openai"
)

// ErrDisabled : llm.enabled=false. L'appelant masque alors les fonctions d'IA.
var ErrDisabled = errors.New("llm: désactivé (llm.enabled=false)")

// Build : la pile complète. rdb porte le plafond de dépense par organisation
// (obligatoire : sans compteur partagé, pas de plafond fiable).
func Build(cfg configs.LLMConfig, rdb redis.UniversalClient) (llm.Provider, error) {
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	ledger, err := llm.NewRedisLedger(rdb, cfg.DailyBudgetMicroUSD)
	if err != nil {
		return nil, err
	}
	return build(cfg, ledger)
}

func build(cfg configs.LLMConfig, ledger llm.Ledger) (llm.Provider, error) {
	slots := []struct {
		label string
		p     configs.LLMProviderConfig
	}{{"primary", cfg.Primary}, {"fallback", cfg.Fallback}}

	breaker := llm.BreakerConfig{Failures: cfg.BreakerFailures, Cooldown: cfg.BreakerCooldown}
	var chain []llm.Provider
	names := map[string]bool{}
	for _, s := range slots {
		if s.p.BaseURL == "" && s.p.Kind == "" {
			if s.label == "primary" {
				return nil, errors.New("llm: llm.primary.base_url (ou kind) est obligatoire")
			}
			continue
		}
		p, err := provider(s.label, s.p)
		if err != nil {
			return nil, err
		}
		if names[p.Name()] {
			return nil, fmt.Errorf("llm: deux fournisseurs nommés %q (labels de métriques ambigus)", p.Name())
		}
		names[p.Name()] = true
		pricing := llm.Pricing{InputMicroUSDPerMTok: s.p.InputMicroUSDPerMTok, OutputMicroUSDPerMTok: s.p.OutputMicroUSDPerMTok}
		chain = append(chain, llm.WithBreaker(llm.WithBudget(p, ledger, pricing), breaker))
	}
	c, err := llm.NewChain(chain...)
	if err != nil {
		return nil, err
	}
	return llm.Instrument(c), nil
}

// Kinds de fournisseur.
const (
	KindOpenAI    = "openai"
	KindAnthropic = "anthropic"
)

func provider(label string, p configs.LLMProviderConfig) (llm.Provider, error) {
	if p.Model == "" {
		return nil, fmt.Errorf("llm.%s.model est obligatoire", label)
	}
	if p.InputMicroUSDPerMTok < 0 || p.OutputMicroUSDPerMTok < 0 {
		return nil, fmt.Errorf("llm.%s : prix négatif", label)
	}
	name := p.Name
	if name == "" {
		name = label
	}
	switch p.Kind {
	case "", KindOpenAI:
	case KindAnthropic:
		if p.BaseURL != "" {
			if err := checkURL(label, p.BaseURL); err != nil {
				return nil, err
			}
		}
		if p.MaxTokensField != "" || p.ExtraBodyJSON != "" {
			return nil, fmt.Errorf("llm.%s : max_tokens_field et extra_body_json ne s'appliquent pas à l'API Claude", label)
		}
		c, err := anthropic.New(anthropic.Config{
			Name: name, APIKey: p.APIKey, Model: p.Model, Effort: p.Effort,
			BaseURL: p.BaseURL, Timeout: p.Timeout,
		})
		if err != nil {
			return nil, fmt.Errorf("llm.%s : %w", label, err)
		}
		return c, nil
	default:
		return nil, fmt.Errorf("llm.%s.kind inconnu : %q (openai ou anthropic)", label, p.Kind)
	}
	if err := checkURL(label, p.BaseURL); err != nil {
		return nil, err
	}
	if p.Effort != "" {
		return nil, fmt.Errorf("llm.%s.effort ne s'applique qu'à l'API Claude", label)
	}
	var extra map[string]any
	if p.ExtraBodyJSON != "" {
		if err := json.Unmarshal([]byte(p.ExtraBodyJSON), &extra); err != nil {
			return nil, fmt.Errorf("llm.%s.extra_body_json : objet JSON attendu : %w", label, err)
		}
	}
	return openai.New(openai.Config{
		Name: name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model,
		Timeout: p.Timeout, MaxTokensField: p.MaxTokensField, ExtraBody: extra,
	})
}

func checkURL(label, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("llm.%s.base_url invalide", label)
	}
	return nil
}
