package llmsetup

import (
	"errors"
	"testing"

	"github.com/thekrauss/kubemanager/internal/core/configs"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

func TestBuild(t *testing.T) {
	if _, err := Build(configs.LLMConfig{}, nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("désactivé : %v", err)
	}
	ledger := llm.NewMemoryLedger(1)
	ok := configs.LLMConfig{Enabled: true,
		Primary:  configs.LLMProviderConfig{Name: "qwen", BaseURL: "http://vllm:8000/v1", Model: "q", ExtraBodyJSON: `{"chat_template_kwargs":{"enable_thinking":false}}`},
		Fallback: configs.LLMProviderConfig{Name: "groq", BaseURL: "https://api.groq.com/openai/v1", Model: "g", APIKey: "k"},
	}
	p, err := build(ok, ledger)
	if err != nil || p.Name() != "qwen>groq" {
		t.Fatalf("pile : %v %v", p, err)
	}

	bad := map[string]func(*configs.LLMConfig){
		"sans principal":    func(c *configs.LLMConfig) { c.Primary.BaseURL = "" },
		"url invalide":      func(c *configs.LLMConfig) { c.Primary.BaseURL = "vllm:8000" },
		"sans modèle":       func(c *configs.LLMConfig) { c.Fallback.Model = "" },
		"extra non objet":   func(c *configs.LLMConfig) { c.Primary.ExtraBodyJSON = `[1]` },
		"noms identiques":   func(c *configs.LLMConfig) { c.Fallback.Name = "qwen" },
		"prix négatif":      func(c *configs.LLMConfig) { c.Primary.InputMicroUSDPerMTok = -1 },
		"champ max inconnu": func(c *configs.LLMConfig) { c.Primary.MaxTokensField = "max" },
	}
	for name, mut := range bad {
		c := ok
		mut(&c)
		if _, err := build(c, ledger); err == nil {
			t.Fatalf("%s : erreur attendue", name)
		}
	}
	claude := ok
	claude.Primary = configs.LLMProviderConfig{Name: "claude", Kind: KindAnthropic, Model: "claude-opus-5-5", APIKey: "k", Effort: "high"}
	if p, err := build(claude, ledger); err != nil || p.Name() != "claude>groq" {
		t.Fatalf("pile claude : %v %v", p, err)
	}
	badClaude := map[string]func(*configs.LLMProviderConfig){
		"kind inconnu":       func(p *configs.LLMProviderConfig) { p.Kind = "bedrock" },
		"claude sans clé":    func(p *configs.LLMProviderConfig) { p.APIKey = "" },
		"effort inconnu":     func(p *configs.LLMProviderConfig) { p.Effort = "turbo" },
		"extra sur claude":   func(p *configs.LLMProviderConfig) { p.ExtraBodyJSON = `{}` },
		"url claude":         func(p *configs.LLMProviderConfig) { p.BaseURL = "api" },
		"effort hors claude": func(p *configs.LLMProviderConfig) { p.Kind = KindOpenAI; p.BaseURL = "http://x"; p.Effort = "low" },
	}
	for name, mut := range badClaude {
		c := claude
		mut(&c.Primary)
		if _, err := build(c, ledger); err == nil {
			t.Fatalf("%s : erreur attendue", name)
		}
	}
	if _, err := Build(ok, nil); err == nil {
		t.Fatal("sans Redis : erreur attendue (pas de plafond sans compteur partagé)")
	}
}
