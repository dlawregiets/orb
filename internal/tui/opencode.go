package tui

import (
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"orb/internal/opencode"
)

func saveConfig(cfg *opencode.Config) tea.Cmd {
	return func() tea.Msg {
		if err := cfg.Save(); err != nil {
			return savedMsg{err: err}
		}
		return savedMsg{warnings: cfg.Warnings()}
	}
}

func (m *Model) currentModelID() string {
	switch m.view {
	case viewDetail:
		if m.selectedIdx >= 0 && m.selectedIdx < len(m.filtered) {
			return m.filtered[m.selectedIdx].ID
		}
	case viewRouting:
		return m.routing.modelID
	}
	row := m.table.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

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

func (m *Model) allowedCell(id string) string {
	if m.allowed[id] {
		return "yes"
	}
	return "-"
}

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
