package tui

import (
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"orb/internal/opencode"
)

// saveConfig writes the config to disk and reports the result as a
// savedMsg, including any warnings about dangling model references.
func saveConfig(cfg *opencode.Config) tea.Cmd {
	return func() tea.Msg {
		if err := cfg.Save(); err != nil {
			return savedMsg{err: err}
		}
		return savedMsg{warnings: cfg.Warnings()}
	}
}

// currentModelID returns the ID of the model the user is currently looking
// at, whether that is a table row, the detail view or the routing view.
func (m *Model) currentModelID() string {
	switch m.view {
	case viewDetail:
		if m.selectedIdx >= 0 && m.selectedIdx < len(m.filtered) {
			return m.filtered[m.selectedIdx].ID
		}
	case viewRouting:
		return m.routing.modelID
	case viewAgents:
		return m.agents.modelID
	}
	row := m.table.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

// toggleSelectedAllowed flips the selected model's membership in the
// allowlist and refreshes the table. When the allowed-only filter would
// hide the model being removed, the detail view closes.
func (m *Model) toggleSelectedAllowed() {
	id := m.currentModelID()
	if id == "" {
		return
	}

	allowed := !m.cfg.Allowed(id)
	m.cfg.SetAllowed(id, allowed)
	m.allowed = m.cfg.AllowedIDs()
	m.applyFilters()
	m.statusIsError = false

	if allowed {
		m.status = "Allowed " + id
	} else {
		m.status = "Removed " + id + " from the allowlist"
	}

	if m.view == viewDetail && m.allowedOnly && !allowed {
		m.view = viewList
	}
}

// allowedCell renders the table's On column.
func (m *Model) allowedCell(id string) string {
	if m.allowed[id] {
		return "yes"
	}
	return "-"
}

// shortenPath replaces the user's home prefix with ~ for display.
func shortenPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}
