package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	tea "github.com/charmbracelet/bubbletea"

	"orb/internal/opencode"
)

func testModel(t *testing.T) (*Model, *opencode.Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(path, []byte(`{"$schema":"https://opencode.ai/config.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := opencode.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := New(nil, cfg, false)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.Update(modelsLoadedMsg{models: []components.Model{
		{ID: "~deepseek/deepseek-flash-latest", Name: "DeepSeek Flash Latest"},
	}})
	return m, cfg
}

func key(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func TestToggleAllowedAndSave(t *testing.T) {
	m, cfg := testModel(t)
	id := "~deepseek/deepseek-flash-latest"

	if cfg.Allowed(id) {
		t.Fatal("model should start disallowed")
	}

	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeySpace}); cmd != nil {
		t.Fatal("toggling should not return a command")
	}
	if !cfg.Allowed(id) {
		t.Fatal("space did not allow the model")
	}
	if !cfg.Dirty() {
		t.Fatal("toggle did not mark the config dirty")
	}

	updated, cmd := m.Update(key('s'))
	if cmd == nil {
		t.Fatal("save did not return a command")
	}
	msg, ok := cmd().(savedMsg)
	if !ok || msg.err != nil {
		t.Fatalf("save failed: %#v", msg)
	}
	if cfg.Dirty() {
		t.Fatal("save did not clear the dirty flag")
	}
	if updated.(*Model).status == "" {
		t.Fatal("save did not set a status message")
	}
}

func TestRoutingEditorAddsAndReorders(t *testing.T) {
	m, cfg := testModel(t)
	id := "~deepseek/deepseek-flash-latest"

	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeySpace}); cmd != nil {
		t.Fatal("unexpected command")
	}
	if cmd := m.openRouting(id, viewList); cmd == nil {
		t.Fatal("openRouting should fetch endpoints")
	}
	m.routing.providers = []routingProvider{
		{Slug: "deepinfra", Name: "DeepInfra"},
		{Slug: "inference-net", Name: "InferenceNet"},
	}
	m.routing.loading = false
	m.refreshRoutingRows()

	// Select the first provider and add it to the order.
	m.routing.table.SetCursor(0)
	m.updateRouting(tea.KeyMsg{Type: tea.KeySpace})
	m.routing.table.SetCursor(1)
	m.updateRouting(tea.KeyMsg{Type: tea.KeySpace})

	routing := cfg.Routing(id)
	if len(routing.Order) != 2 {
		t.Fatalf("unexpected order: %v", routing.Order)
	}
	if routing.AllowFallbacks == false {
		t.Fatal("fallbacks should default to on")
	}

	m.updateRouting(key('K'))
	routing = cfg.Routing(id)
	if routing.Order[0] != "inference-net" {
		t.Fatalf("reorder failed: %v", routing.Order)
	}

	m.updateRouting(key('a'))
	if cfg.Routing(id).AllowFallbacks {
		t.Fatal("fallbacks were not disabled")
	}
}

func TestRoutingRequiresAllowedModel(t *testing.T) {
	m, _ := testModel(t)
	id := "~deepseek/deepseek-flash-latest"
	if cmd := m.openRouting(id, viewList); cmd != nil {
		t.Fatal("opening routing for a disallowed model should not fetch")
	}
	if !m.statusIsError {
		t.Fatal("expected an error status")
	}
}

func TestManualRoutingProvider(t *testing.T) {
	m, cfg := testModel(t)
	id := "~deepseek/deepseek-flash-latest"
	m.cfg.SetAllowed(id, true)
	m.allowed = m.cfg.AllowedIDs()
	m.openRouting(id, viewList)

	m.updateRouting(key('i'))
	for _, r := range "deepseek" {
		m.updateRouting(key(r))
	}
	m.updateRouting(tea.KeyMsg{Type: tea.KeyEnter})

	if got := cfg.Routing(id).Order; len(got) != 1 || got[0] != "deepseek" {
		t.Fatalf("unexpected order: %v", got)
	}
}
