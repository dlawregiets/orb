package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestAgentAssignmentView(t *testing.T) {
	m, cfg := testModel(t)
	id := "~deepseek/deepseek-flash-latest"

	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeySpace}); cmd != nil {
		t.Fatal("unexpected command")
	}

	updated, _ := m.Update(key('a'))
	m = updated.(*Model)
	if m.view != viewAgents {
		t.Fatalf("expected the agent view, got %v", m.view)
	}

	m.Update(key('i'))
	for _, r := range "reviewer" {
		m.Update(key(r))
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.agents.names) != 1 || m.agents.names[0] != "reviewer" {
		t.Fatalf("agent was not added: %v", m.agents.names)
	}

	m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if !cfg.AgentUsesModel("reviewer", id) {
		t.Fatal("space did not assign the model to the agent")
	}
	if !cfg.Dirty() {
		t.Fatal("assignment did not mark the config dirty")
	}

	m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if cfg.AgentUsesModel("reviewer", id) {
		t.Fatal("space did not remove the assignment")
	}
	if len(cfg.Agents()) != 0 {
		t.Fatalf("empty agent was left in the config: %v", cfg.Agents())
	}
}

func TestAgentViewRequiresAllowedModel(t *testing.T) {
	m, _ := testModel(t)
	id := "~deepseek/deepseek-flash-latest"

	if cmd := m.openAgents(id, viewList); cmd != nil {
		t.Fatal("opening agents for a disallowed model should not fetch")
	}
	if !m.statusIsError {
		t.Fatal("expected an error status")
	}
	if m.view != viewList {
		t.Fatalf("unexpected view: %v", m.view)
	}
}

func TestAgentViewEscapeReturns(t *testing.T) {
	m, cfg := testModel(t)
	id := "~deepseek/deepseek-flash-latest"
	m.cfg.SetAllowed(id, true)
	m.allowed = m.cfg.AllowedIDs()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	m.openAgents(id, viewDetail)
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.view != viewDetail {
		t.Fatalf("escape did not return to the detail view: %v", m.view)
	}
	if cfg.Dirty() {
		t.Fatal("opening and closing the agent view should not modify the config")
	}
}
