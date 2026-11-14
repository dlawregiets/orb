package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// agentState holds the transient state of the agent assignment picker for the
// model currently being configured.
type agentState struct {
	modelID     string
	returnView  view
	names       []string
	cursor      int
	input       textinput.Model
	inputActive bool
}

// newAgentState builds the picker with its agent-name input.
func newAgentState() agentState {
	input := textinput.New()
	input.Placeholder = "agent name"
	input.CharLimit = 60
	input.Width = 30
	return agentState{input: input}
}

// openAgents enters the agent assignment view for a model, listing the agents
// already defined in the configuration.
func (m *Model) openAgents(modelID string, returnView view) tea.Cmd {
	if !m.cfg.Allowed(modelID) {
		m.status = "Allow " + modelID + " first with space"
		m.statusIsError = true
		return nil
	}

	m.agents = agentState{
		modelID:    modelID,
		returnView: returnView,
		names:      m.cfg.Agents(),
		input:      m.agents.input,
	}
	m.agents.input.SetValue("")
	m.agents.input.Blur()
	m.stopMarquee()
	m.view = viewAgents
	m.status = ""
	m.statusIsError = false
	return nil
}

// selectedAgent returns the agent name under the cursor, or "".
func (m *Model) selectedAgent() string {
	if m.agents.cursor < 0 || m.agents.cursor >= len(m.agents.names) {
		return ""
	}
	return m.agents.names[m.agents.cursor]
}

// moveAgentCursor shifts the selection, clamped to the valid range.
func (m *Model) moveAgentCursor(delta int) {
	if len(m.agents.names) == 0 {
		m.agents.cursor = 0
		return
	}
	m.agents.cursor = min(max(m.agents.cursor+delta, 0), len(m.agents.names)-1)
}

// toggleAgentAssignment assigns the current model to the selected agent, or
// removes the assignment when the agent already uses it.
func (m *Model) toggleAgentAssignment() {
	name := m.selectedAgent()
	if name == "" {
		return
	}

	if m.cfg.AgentUsesModel(name, m.agents.modelID) {
		m.cfg.ClearAgentModel(name)
		m.status = "Removed " + m.agents.modelID + " from agent " + name
	} else {
		m.cfg.SetAgentModel(name, m.agents.modelID)
		m.status = "Assigned " + m.agents.modelID + " to agent " + name
	}
	m.allowed = m.cfg.AllowedIDs()
	m.statusIsError = false
}

// addAgent appends a manually entered agent name to the picker. The name is
// only written to the configuration once it is assigned a model.
func (m *Model) addAgent(value string) {
	name := strings.TrimSpace(value)
	if name == "" {
		return
	}
	for _, r := range name {
		if !isProviderSlugChar(r) {
			m.status = "Invalid agent name: " + name
			m.statusIsError = true
			return
		}
	}

	if indexOf(m.agents.names, name) < 0 {
		m.agents.names = append(m.agents.names, name)
		sort.Strings(m.agents.names)
	}
	m.agents.cursor = indexOf(m.agents.names, name)
	m.status = "Select " + name + " and press space to assign the model"
	m.statusIsError = false
}

// updateAgents handles keys while the agent assignment view is open. When the
// add-agent input is focused it captures keys; otherwise they drive selection,
// assignment and saving.
func (m *Model) updateAgents(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.agents.inputActive {
		switch msg.String() {
		case "esc":
			m.agents.input.Blur()
			m.agents.inputActive = false
			m.agents.input.SetValue("")
			return m, nil
		case "enter":
			m.addAgent(m.agents.input.Value())
			m.agents.input.Blur()
			m.agents.inputActive = false
			m.agents.input.SetValue("")
			return m, nil
		}
		newInput, cmd := m.agents.input.Update(msg)
		m.agents.input = newInput
		return m, cmd
	}

	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit

	case "esc", "q":
		m.view = m.agents.returnView
		return m, m.maybeStartMarquee()

	case "s":
		return m, saveConfig(m.cfg)

	case " ":
		m.toggleAgentAssignment()
		return m, nil

	case "i":
		m.agents.input.Focus()
		m.agents.inputActive = true
		return m, textinput.Blink

	case "up", "k":
		m.moveAgentCursor(-1)
		return m, nil

	case "down", "j":
		m.moveAgentCursor(1)
		return m, nil

	case "pgup", "b":
		m.moveAgentCursor(-8)
		return m, nil

	case "pgdown", "f":
		m.moveAgentCursor(8)
		return m, nil

	case "home", "g":
		m.agents.cursor = 0
		return m, nil

	case "end", "G":
		if len(m.agents.names) > 0 {
			m.agents.cursor = len(m.agents.names) - 1
		}
		return m, nil
	}

	return m, nil
}

// agentView renders the agent assignment picker.
func (m *Model) agentView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(" Agents: " + m.agents.modelID + " "))
	fmt.Fprint(&b, "\n\n")
	b.WriteString(helpStyle.Render(" Assign this model to one or more agents. Space toggles the selection; i adds an agent by name."))
	fmt.Fprint(&b, "\n\n")

	if len(m.agents.names) == 0 {
		b.WriteString(helpStyle.Render(" No agents are configured yet. Press i to add one."))
		fmt.Fprint(&b, "\n")
	}

	for i, name := range m.agents.names {
		marker := "  "
		if i == m.agents.cursor {
			marker = "> "
		}

		state := "-"
		switch {
		case m.cfg.AgentUsesModel(name, m.agents.modelID):
			state = "assigned"
		default:
			if ref, ok := m.cfg.AgentModel(name); ok {
				state = ref
			}
		}

		line := truncateLine(fmt.Sprintf("%s%-24s %s", marker, name, state), m.width-4)
		if i == m.agents.cursor {
			line = filterStyle.Render(line)
		}
		b.WriteString(" " + line)
		fmt.Fprint(&b, "\n")
	}

	if m.agents.inputActive {
		fmt.Fprintf(&b, "\n %s %s\n", filterStyle.Render("Add agent:"), m.agents.input.View())
	}

	b.WriteString(helpStyle.Render("\n space assign/unassign  i add agent  s save  esc/q back"))
	if m.status != "" {
		fmt.Fprint(&b, "\n")
		if m.statusIsError {
			b.WriteString(errorStyle.Render(" " + m.status))
		} else {
			b.WriteString(keyStyle.Render(" " + m.status))
		}
	}
	return b.String()
}

// truncateLine shortens text to at most limit runes, marking the cut with an
// ellipsis. A non-positive limit leaves the text alone.
func truncateLine(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}
