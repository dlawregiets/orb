package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/hujson"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLoadSeedsFromRawProvider(t *testing.T) {
	path := writeConfig(t, `{
  "model": "openrouter/~deepseek/deepseek-flash-latest",
  "providers": {
    "openrouter": {
      "models": {
        "~deepseek/deepseek-flash-latest": {
          "body": {"provider": {"order": ["inference-net"], "allow_fallbacks": false}}
        },
        "~z-ai/glm-flash-latest": {},
        "~openai/gpt-latest": {"disabled": true}
      }
    }
  }
}`)

	cfg := load(t, path)
	if !cfg.Allowed("~deepseek/deepseek-flash-latest") || !cfg.Allowed("~z-ai/glm-flash-latest") {
		t.Fatalf("expected both models to be allowed, got %v", cfg.AllowedIDs())
	}
	if cfg.Allowed("openai/gpt-5") {
		t.Fatal("unexpected allowed model")
	}
	if cfg.Allowed("~openai/gpt-latest") {
		t.Fatal("disabled entries should not be seeded")
	}
	if cfg.Dirty() {
		t.Fatal("loading should not mark the config dirty")
	}

	routing := cfg.Routing("~deepseek/deepseek-flash-latest")
	if !routing.Configured || routing.AllowFallbacks {
		t.Fatalf("unexpected routing: %#v", routing)
	}
	if len(routing.Order) != 1 || routing.Order[0] != "inference-net" {
		t.Fatalf("unexpected order: %v", routing.Order)
	}
}

func TestSaveCreatesCuratedProviderAndRewritesReferences(t *testing.T) {
	path := writeConfig(t, `{
  // keep this comment
  "$schema": "https://opencode.ai/config.json",
  "model": "openrouter/~z-ai/glm-flash-latest#high",
  "agents": {
    "reviewer": {"model": "openrouter/~z-ai/glm-flash-latest"}
  },
  "commands": {
    "explain": {"model": "openrouter/~z-ai/glm-flash-latest", "template": "explain"}
  },
  "providers": {
    "openrouter": {
      "headers": {"X-Test": "yes"},
      "models": {
        "~deepseek/deepseek-flash-latest": {
          "body": {"provider": {"order": ["inference-net", "deepinfra"], "allow_fallbacks": false}}
        },
        "~z-ai/glm-flash-latest": {}
      }
    }
  }
}`)

	cfg := load(t, path)
	cfg.SetAllowed("~deepseek/deepseek-flash-latest", false)
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out := read(t, path)
	if !strings.Contains(out, "// keep this comment") {
		t.Fatalf("comment was not preserved:\n%s", out)
	}

	doc, err := hujson.Parse([]byte(out))
	if err != nil {
		t.Fatalf("saved config is invalid HuJSON: %v\n%s", err, out)
	}
	std := doc.Clone()
	std.Standardize()
	var raw map[string]any
	if err := json.Unmarshal(std.Pack(), &raw); err != nil {
		t.Fatal(err)
	}

	if got := raw["model"]; got != "openrouter-curated/~z-ai/glm-flash-latest#high" {
		t.Fatalf("unexpected model: %v", got)
	}

	agents := raw["agents"].(map[string]any)
	reviewer := agents["reviewer"].(map[string]any)
	if got := reviewer["model"]; got != "openrouter-curated/~z-ai/glm-flash-latest" {
		t.Fatalf("unexpected agent model: %v", got)
	}
	commands := raw["commands"].(map[string]any)
	explain := commands["explain"].(map[string]any)
	if got := explain["model"]; got != "openrouter-curated/~z-ai/glm-flash-latest" {
		t.Fatalf("unexpected command model: %v", got)
	}

	providers := raw["providers"].(map[string]any)
	curated := providers[CuratedProvider].(map[string]any)
	if curated["canonical"] != RawProvider {
		t.Fatalf("missing canonical provider: %v", curated)
	}
	if headers := curated["headers"].(map[string]any); headers["X-Test"] != "yes" {
		t.Fatalf("provider headers were not copied: %v", headers)
	}
	env := curated["env"].([]any)
	if len(env) != 1 || env[0] != "OPENROUTER_API_KEY" {
		t.Fatalf("provider env was not set: %v", env)
	}
	models := curated["models"].(map[string]any)
	if _, ok := models["~deepseek/deepseek-flash-latest"]; ok {
		t.Fatalf("disallowed model was saved: %v", models)
	}
	if _, ok := models["~z-ai/glm-flash-latest"]; !ok {
		t.Fatalf("allowed model missing: %v", models)
	}

	policies := raw["policies"].([]any)
	if len(policies) != 2 {
		t.Fatalf("unexpected policies: %v", policies)
	}
	if !hasPolicy(policies, RawProvider, "deny") || !hasPolicy(policies, CuratedProvider, "allow") {
		t.Fatalf("unexpected policies: %v", policies)
	}
}

func TestSavePreservesUnrelatedPoliciesAndDeduplicates(t *testing.T) {
	path := writeConfig(t, `{
  "$schema": "https://opencode.ai/config.json",
  "policies": [
    {"action": "provider.use", "resource": "*", "effect": "deny"},
    {"action": "provider.use", "resource": "openrouter", "effect": "allow"},
    {"action": "provider.use", "resource": "openrouter-curated", "effect": "deny"},
    {"action": "tool.use", "resource": "shell:*", "effect": "deny"}
  ]
}`)

	cfg := load(t, path)
	cfg.SetAllowed("~deepseek/deepseek-flash-latest", true)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	cfg = load(t, path)
	cfg.SetAllowed("~deepseek/deepseek-flash-latest", false)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	out := read(t, path)
	if strings.Count(out, `"resource": "openrouter"`) != 1 {
		t.Fatalf("openrouter policy duplicated:\n%s", out)
	}
	if strings.Count(out, `"resource": "openrouter-curated"`) != 1 {
		t.Fatalf("curated policy duplicated:\n%s", out)
	}
	if !strings.Contains(out, `"resource": "shell:*"`) || !strings.Contains(out, `"resource": "*"`) {
		t.Fatalf("unrelated policies were dropped:\n%s", out)
	}
}

func TestRoutingIsPreservedWhenToggling(t *testing.T) {
	path := writeConfig(t, `{"$schema": "https://opencode.ai/config.json"}`)
	cfg := load(t, path)

	cfg.SetAllowed("~deepseek/deepseek-flash-latest", true)
	cfg.SetRouting("~deepseek/deepseek-flash-latest", Routing{
		Order:          []string{"inference-net", "deepinfra"},
		AllowFallbacks: false,
	})
	cfg.SetAllowed("~deepseek/deepseek-flash-latest", false)
	cfg.SetAllowed("~deepseek/deepseek-flash-latest", true)

	routing := cfg.Routing("~deepseek/deepseek-flash-latest")
	if len(routing.Order) != 2 || routing.Order[0] != "inference-net" || routing.Order[1] != "deepinfra" {
		t.Fatalf("unexpected routing after toggling: %#v", routing)
	}

	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	cfg = load(t, path)
	routing = cfg.Routing("~deepseek/deepseek-flash-latest")
	if !routing.Configured || routing.AllowFallbacks || len(routing.Order) != 2 {
		t.Fatalf("unexpected routing after reload: %#v", routing)
	}
}

func TestCuratedProviderWinsOverRawModels(t *testing.T) {
	path := writeConfig(t, `{
  "$schema": "https://opencode.ai/config.json",
  "providers": {
    "openrouter": {"models": {"~deepseek/deepseek-flash-latest": {}}},
    "openrouter-curated": {"models": {}}
  }
}`)

	cfg := load(t, path)
	if cfg.Allowed("~deepseek/deepseek-flash-latest") {
		t.Fatal("raw models should not seed an existing curated provider")
	}
}

func TestNewFileGetsSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "opencode.json")
	cfg := load(t, path)
	cfg.SetAllowed("~deepseek/deepseek-flash-latest", true)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	out := read(t, path)
	if !strings.Contains(out, SchemaURL) {
		t.Fatalf("schema missing:\n%s", out)
	}
	cfg = load(t, path)
	if !cfg.Allowed("~deepseek/deepseek-flash-latest") {
		t.Fatal("model not loaded after creating config")
	}
}

func TestSecondSaveIsNoop(t *testing.T) {
	path := writeConfig(t, `{"$schema": "https://opencode.ai/config.json"}`)
	cfg := load(t, path)
	cfg.SetAllowed("~deepseek/deepseek-flash-latest", true)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	first := read(t, path)

	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if second := read(t, path); first != second {
		t.Fatalf("second save changed the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func hasPolicy(policies []any, resource, effect string) bool {
	for _, value := range policies {
		statement, ok := value.(map[string]any)
		if ok && statement["action"] == providerPolicy && statement["resource"] == resource && statement["effect"] == effect {
			return true
		}
	}
	return false
}
