package tui

import "github.com/charmbracelet/lipgloss"

var (
	baseStyle   = lipgloss.NewStyle().Margin(0, 2)
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7C3AED")).MarginBottom(1)
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#A78BFA"))
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280"))
	filterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#F59E0B"))
	keyStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#10B981"))
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444")).MarginTop(1)
	detailStyle = lipgloss.NewStyle().Margin(1, 2)
)
