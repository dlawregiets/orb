// Package tui provides a terminal UI for browsing OpenRouter models and
// curating the models allowed by an OpenCode configuration.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"orb/internal/opencode"
)

type view int

const (
	viewList view = iota
	viewDetail
	viewRouting
)

type modelsLoadedMsg struct {
	models []components.Model
}

type errMsg struct {
	err error
}

type savedMsg struct {
	err      error
	warnings []string
}

func fetchModels(client *openrouter.OpenRouter) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := client.Models.List(ctx, nil)
		if err != nil {
			return errMsg{err}
		}
		return modelsLoadedMsg{models: res.GetResult().Data}
	}
}

var modalities = []string{"all", "text", "image", "audio", "video"}

// Model is the main bubbletea model for the TUI.
type Model struct {
	client        *openrouter.OpenRouter
	cfg           *opencode.Config
	authenticated bool
	models        []components.Model
	filtered      []components.Model
	allowed       map[string]bool
	err           error
	width         int
	height        int
	view          view
	loading       bool

	table       table.Model
	searchInput textinput.Model
	cutoffInput textinput.Model

	selectedIdx   int
	freeOnly      bool
	allowedOnly   bool
	modalityIdx   int
	cutoffEnabled bool
	ready         bool

	routing routingState

	status        string
	statusIsError bool
	confirmQuit   bool
}

// New creates a new Model with the given OpenRouter client and OpenCode config.
// authenticated reports whether the client sends an API key, which OpenRouter
// requires for latency and throughput statistics.
func New(client *openrouter.OpenRouter, cfg *opencode.Config, authenticated bool) *Model {
	ti := textinput.New()
	ti.Placeholder = "Type to filter..."
	ti.CharLimit = 100
	ti.Width = 30

	ci := textinput.New()
	ci.Placeholder = "Year (e.g. 2024)"
	ci.CharLimit = 4
	ci.Width = 8

	columns := []table.Column{
		{Title: "Model", Width: 60},
		{Title: "On", Width: 4},
		{Title: "Provider", Width: 12},
		{Title: "Context", Width: 9},
		{Title: "Prompt/M", Width: 11},
		{Title: "Comp/M", Width: 11},
		{Title: "Modality", Width: 13},
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(20),
	)

	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("#3730A3")).
		BorderBottom(true).
		Bold(true).
		Foreground(lipgloss.Color("#A78BFA"))
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("#FFF")).
		Background(lipgloss.Color("#4C1D95")).
		Bold(false)
	t.SetStyles(s)

	ri := textinput.New()
	ri.Placeholder = "provider slug"
	ri.CharLimit = 60
	ri.Width = 30

	return &Model{
		client:        client,
		cfg:           cfg,
		authenticated: authenticated,
		allowed:       cfg.AllowedIDs(),
		searchInput:   ti,
		cutoffInput:   ci,
		table:         t,
		loading:       true,
		routing:       routingState{input: ri},
	}
}

// Init initializes the model by fetching models.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		fetchModels(m.client),
		textinput.Blink,
	)
}

// Update handles messages and user input.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true

		h := msg.Height - 12
		if h < 5 {
			h = 5
		}
		m.table.SetHeight(h)

		w := msg.Width - 6
		modelW := w * 4 / 10
		onW := 4
		if modelW < 40 {
			modelW = 40
		}
		small := (w - modelW - onW) / 5
		if small < 8 {
			small = 8
			modelW = w - onW - small*5
		}
		m.table.SetColumns([]table.Column{
			{Title: "Model", Width: modelW},
			{Title: "On", Width: onW},
			{Title: "Provider", Width: small},
			{Title: "Context", Width: small - 2},
			{Title: "Prompt/M", Width: small},
			{Title: "Comp/M", Width: small},
			{Title: "Modality", Width: small - 2},
		})
		m.resizeRouting(msg.Height, msg.Width)

	case errMsg:
		m.err = msg.err
		m.loading = false
		return m, nil

	case modelsLoadedMsg:
		m.models = msg.models
		m.loading = false
		m.applyFilters()

	case savedMsg:
		if msg.err != nil {
			m.status = "Save failed: " + msg.err.Error()
			m.statusIsError = true
			return m, nil
		}
		m.status = "Saved " + shortenPath(m.cfg.Path)
		m.statusIsError = false
		if len(msg.warnings) > 0 {
			m.status = "Saved with warnings: " + strings.Join(msg.warnings, "; ")
			m.statusIsError = true
		}

	case endpointsLoadedMsg:
		m.handleEndpoints(msg)

	case tea.KeyMsg:
		if m.view == viewRouting {
			return m.updateRouting(msg)
		}

		if m.view == viewDetail {
			switch msg.String() {
			case "esc", "q", "enter":
				m.view = viewList
				return m, nil
			case " ":
				m.toggleSelectedAllowed()
				return m, nil
			case "p":
				if id := m.currentModelID(); id != "" {
					return m, m.openRouting(id, viewDetail)
				}
				return m, nil
			case "s":
				return m, saveConfig(m.cfg)
			}
			return m, nil
		}

		if m.cutoffInput.Focused() {
			newInput, cmd := m.cutoffInput.Update(msg)
			m.cutoffInput = newInput
			if msg.Type == tea.KeyBackspace || msg.Type == tea.KeyDelete || msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				m.cutoffEnabled = m.cutoffInput.Value() != ""
				m.applyFilters()
			}
			if msg.String() == "enter" || msg.String() == "esc" {
				m.cutoffInput.Blur()
				m.cutoffEnabled = m.cutoffInput.Value() != ""
				m.applyFilters()
				return m, nil
			}
			return m, cmd
		}

		if m.searchInput.Focused() {
			switch msg.String() {
			case "enter", "esc":
				m.searchInput.Blur()
				return m, nil
			}
			newInput, cmd := m.searchInput.Update(msg)
			m.searchInput = newInput
			if msg.Type == tea.KeyBackspace || msg.Type == tea.KeyDelete || msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				m.applyFilters()
			}
			return m, cmd
		}

		if m.confirmQuit {
			switch msg.String() {
			case "q":
				return m, tea.Quit
			case "s":
				m.confirmQuit = false
				return m, saveConfig(m.cfg)
			case "esc":
				m.confirmQuit = false
				m.status = ""
				m.statusIsError = false
			}
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "q":
			if m.cfg.Dirty() {
				m.confirmQuit = true
				m.status = "Unsaved changes: s save, q discard, esc cancel"
				m.statusIsError = true
				return m, nil
			}
			return m, tea.Quit

		case " ":
			m.toggleSelectedAllowed()
			return m, nil

		case "p":
			if id := m.currentModelID(); id != "" {
				return m, m.openRouting(id, viewList)
			}
			return m, nil

		case "s":
			return m, saveConfig(m.cfg)

		case "o":
			m.allowedOnly = !m.allowedOnly
			m.applyFilters()

		case "/":
			m.searchInput.Focus()
			return m, textinput.Blink

		case "r":
			m.err = nil
			m.loading = true
			return m, fetchModels(m.client)

		case "f":
			m.freeOnly = !m.freeOnly
			m.applyFilters()

		case "m":
			m.modalityIdx = (m.modalityIdx + 1) % len(modalities)
			m.applyFilters()

		case "y":
			m.cutoffInput.Focus()
			return m, textinput.Blink

		case "esc":
			if m.searchInput.Focused() {
				m.searchInput.Blur()
				m.searchInput.SetValue("")
				m.applyFilters()
			}

		case "enter":
			row := m.table.SelectedRow()
			if len(row) > 0 && row[0] != "" {
				id := row[0]
				for i, mo := range m.filtered {
					if mo.ID == id || mo.Name == id {
						m.selectedIdx = i
						m.view = viewDetail
						break
					}
				}
			}
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m *Model) applyFilters() {
	if len(m.models) == 0 {
		m.filtered = nil
		return
	}

	search := strings.ToLower(strings.TrimSpace(m.searchInput.Value()))
	cutoffStr := strings.TrimSpace(m.cutoffInput.Value())

	filtered := make([]components.Model, 0, len(m.models))
	for _, model := range m.models {
		if search != "" {
			id := strings.ToLower(model.ID)
			name := strings.ToLower(model.Name)
			if !strings.Contains(id, search) && !strings.Contains(name, search) {
				continue
			}
		}

		if m.freeOnly {
			promptPrice := parsePrice(model.Pricing.Prompt)
			completionPrice := parsePrice(model.Pricing.Completion)
			if promptPrice > 0 || completionPrice > 0 {
				continue
			}
		}

		if m.allowedOnly && !m.allowed[model.ID] {
			continue
		}

		if m.modalityIdx > 0 {
			targetMod := modalities[m.modalityIdx]
			if model.Architecture.Modality != nil {
				mod := strings.ToLower(*model.Architecture.Modality)
				if !strings.Contains(mod, targetMod) {
					continue
				}
			} else {
				continue
			}
		}

		if cutoffStr != "" {
			targetYear, err := strconv.Atoi(cutoffStr)
			if err == nil && targetYear > 0 {
				if model.KnowledgeCutoff.IsSet() && !model.KnowledgeCutoff.IsNull() {
					val := model.KnowledgeCutoff[true]
					if val != nil && *val != "" {
						parts := strings.SplitN(*val, "-", 2)
						if len(parts) > 0 {
							year, err := strconv.Atoi(parts[0])
							if err == nil && year < targetYear {
								continue
							}
						}
					}
				} else {
					continue
				}
			}
		}

		filtered = append(filtered, model)
	}

	sort.Slice(filtered, func(i, j int) bool {
		ci := safeContext(filtered[i].ContextLength)
		cj := safeContext(filtered[j].ContextLength)
		if ci != cj {
			return ci > cj
		}
		return filtered[i].ID < filtered[j].ID
	})

	m.filtered = filtered

	rows := make([]table.Row, 0, len(filtered))
	for _, model := range filtered {
		provider := extractProvider(model.ID)
		promptPrice := formatPrice(parsePrice(model.Pricing.Prompt))
		completionPrice := formatPrice(parsePrice(model.Pricing.Completion))
		modality := safeStr(model.Architecture.Modality)
		ctxStr := formatContext(model.ContextLength)
		rows = append(rows, table.Row{
			model.ID,
			m.allowedCell(model.ID),
			provider,
			ctxStr,
			promptPrice,
			completionPrice,
			modality,
		})
	}
	m.table.SetRows(rows)
}

// View renders the current view.
func (m *Model) View() string {
	if m.loading {
		return "\n  Fetching models from OpenRouter..."
	}

	if !m.ready {
		return "\n  Initializing..."
	}

	if m.view == viewRouting {
		return m.routingView()
	}

	if m.view == viewDetail {
		return m.detailView()
	}

	return m.listView()
}

func (m *Model) listView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render(" OpenRouter Model Browser "))
	fmt.Fprint(&b, "\n")
	dirty := ""
	if m.cfg.Dirty() {
		dirty = "  " + filterStyle.Render("(modified)")
	}
	fmt.Fprintf(&b, " %s %s%s\n\n", headerStyle.Render("Config:"), shortenPath(m.cfg.Path), dirty)

	searchLabel := filterStyle.Render("Search:")
	if m.searchInput.Focused() {
		fmt.Fprintf(&b, " %s %s\n", searchLabel, m.searchInput.View())
	} else {
		fmt.Fprintf(&b, " %s %s  [%s]\n", searchLabel, m.searchInput.View(), keyStyle.Render("/"))
	}

	fmt.Fprintf(&b, " %s  %s  mod:%s  [%s]",
		checkbox("Free", m.freeOnly, "f"),
		checkbox("Allowed", m.allowedOnly, "o"),
		filterStyle.Render(modalities[m.modalityIdx]),
		keyStyle.Render("m"),
	)

	if m.cutoffInput.Focused() {
		fmt.Fprintf(&b, "  cutoff: %s", m.cutoffInput.View())
	} else if m.cutoffEnabled {
		fmt.Fprintf(&b, "  cutoff: %s  [%s]", filterStyle.Render(m.cutoffInput.Value()), keyStyle.Render("y"))
	} else {
		fmt.Fprintf(&b, "  cutoff: -  [%s]", keyStyle.Render("y"))
	}
	fmt.Fprintf(&b, "  [%s]\n", keyStyle.Render("r"))

	if m.err != nil {
		fmt.Fprintf(&b, "%s\n", errorStyle.Render(fmt.Sprintf(" Error: %v", m.err)))
	}

	fmt.Fprint(&b, "\n")
	b.WriteString(baseStyle.Render(m.table.View()))
	fmt.Fprint(&b, "\n")

	countMsg := fmt.Sprintf(" %d models · %d allowed", len(m.filtered), len(m.allowed))
	if len(m.models) != len(m.filtered) {
		countMsg = fmt.Sprintf(" %d / %d models · %d allowed", len(m.filtered), len(m.models), len(m.allowed))
	}
	b.WriteString(helpStyle.Render(countMsg))
	fmt.Fprint(&b, "\n")
	b.WriteString(helpStyle.Render(" ↑↓ nav  enter detail  space allow  p routing  s save  q quit  / search  f free  o allowed  m modality  y cutoff  r refresh"))
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

func (m *Model) detailView() string {
	if m.selectedIdx >= len(m.filtered) {
		m.view = viewList
		return m.listView()
	}

	model := m.filtered[m.selectedIdx]
	valWidth := m.width - 26
	if valWidth < 30 {
		valWidth = 30
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render(" " + model.Name + " "))
	fmt.Fprint(&b, "\n\n")

	wrapField(&b, "ID", model.ID, valWidth)
	wrapField(&b, "Provider", extractProvider(model.ID), valWidth)

	if m.cfg.Allowed(model.ID) {
		wrapField(&b, "OpenCode", "allowed", valWidth)
	} else {
		wrapField(&b, "OpenCode", "not allowed (space to allow)", valWidth)
	}
	routing := m.cfg.Routing(model.ID)
	if len(routing.Order) > 0 {
		wrapField(&b, "Routing", strings.Join(routing.Order, " → "), valWidth)
	} else if routing.Configured {
		wrapField(&b, "Routing", "automatic", valWidth)
	} else {
		wrapField(&b, "Routing", "default (p to edit)", valWidth)
	}
	if routing.Configured || len(routing.Order) > 0 {
		fallbacks := "on"
		if !routing.AllowFallbacks {
			fallbacks = "off"
		}
		wrapField(&b, "Fallbacks", fallbacks, valWidth)
	}

	modality := "unknown"
	if model.Architecture.Modality != nil {
		modality = *model.Architecture.Modality
	}
	wrapField(&b, "Modality", modality, valWidth)
	wrapField(&b, "Context", formatContext(model.ContextLength), valWidth)

	promptPrice := formatPrice(parsePrice(model.Pricing.Prompt))
	completionPrice := formatPrice(parsePrice(model.Pricing.Completion))
	wrapField(&b, "Price (prompt)", promptPrice+"/1M tokens", valWidth)
	wrapField(&b, "Price (completion)", completionPrice+"/1M tokens", valWidth)

	if model.KnowledgeCutoff.IsSet() && !model.KnowledgeCutoff.IsNull() {
		val := model.KnowledgeCutoff[true]
		if val != nil && *val != "" {
			wrapField(&b, "Knowledge Cutoff", *val, valWidth)
		}
	}

	if model.Description != nil && *model.Description != "" {
		wrapField(&b, "Description", *model.Description, valWidth)
	}

	if model.Created > 0 {
		wrapField(&b, "Created", formatUnix(model.Created), valWidth)
	}

	if model.ContextLength != nil {
		wrapField(&b, "Max Context", formatContext(model.ContextLength)+" tokens", valWidth)
	}

	if model.PerRequestLimits != nil {
		if model.PerRequestLimits.PromptTokens > 0 {
			wrapField(&b, "Max Prompt Tokens", fmt.Sprintf("%.0f", model.PerRequestLimits.PromptTokens), valWidth)
		}
		if model.PerRequestLimits.CompletionTokens > 0 {
			wrapField(&b, "Max Completion Tokens", fmt.Sprintf("%.0f", model.PerRequestLimits.CompletionTokens), valWidth)
		}
	}

	if len(model.SupportedParameters) > 0 {
		params := make([]string, len(model.SupportedParameters))
		for i, p := range model.SupportedParameters {
			params[i] = string(p)
		}
		wrapField(&b, "Supported Params", strings.Join(params, ", "), valWidth)
	}

	if model.Architecture.Tokenizer != nil {
		wrapField(&b, "Tokenizer", string(*model.Architecture.Tokenizer), valWidth)
	}

	if len(model.Architecture.InputModalities) > 0 {
		mods := make([]string, len(model.Architecture.InputModalities))
		for i, mo := range model.Architecture.InputModalities {
			mods[i] = string(mo)
		}
		wrapField(&b, "Input Modalities", strings.Join(mods, ", "), valWidth)
	}

	if len(model.Architecture.OutputModalities) > 0 {
		mods := make([]string, len(model.Architecture.OutputModalities))
		for i, mo := range model.Architecture.OutputModalities {
			mods[i] = string(mo)
		}
		wrapField(&b, "Output Modalities", strings.Join(mods, ", "), valWidth)
	}

	b.WriteString(helpStyle.Render("\n space allow  p routing  s save  esc/q back"))
	if m.status != "" {
		fmt.Fprint(&b, "\n")
		if m.statusIsError {
			b.WriteString(errorStyle.Render(" " + m.status))
		} else {
			b.WriteString(keyStyle.Render(" " + m.status))
		}
	}

	return detailStyle.Render(b.String())
}

func checkbox(label string, checked bool, key string) string {
	if checked {
		return fmt.Sprintf("[x] %s  [%s]", label, keyStyle.Render(key))
	}
	return fmt.Sprintf("[ ] %s  [%s]", label, keyStyle.Render(key))
}

func wrapField(b *strings.Builder, label, value string, maxWidth int) {
	if len(value) <= maxWidth {
		fmt.Fprintf(b, " %s  %s\n", headerStyle.Render(label+":"), value)
		return
	}
	wrapped := wrapText(value, maxWidth)
	lines := strings.Split(wrapped, "\n")
	for i, line := range lines {
		if i == 0 {
			fmt.Fprintf(b, " %s  %s\n", headerStyle.Render(label+":"), line)
		} else {
			fmt.Fprintf(b, "   %s\n", line)
		}
	}
}

func parsePrice(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func formatPrice(price float64) string {
	perM := price * 1_000_000
	switch {
	case perM >= 10:
		return fmt.Sprintf("$%.2f", perM)
	case perM >= 1:
		return fmt.Sprintf("$%.3f", perM)
	case perM > 0:
		return fmt.Sprintf("$%.4f", perM)
	default:
		return "Free"
	}
}

func formatContext(ctx *int64) string {
	if ctx == nil {
		return "?"
	}
	switch {
	case *ctx >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(*ctx)/1_000_000)
	case *ctx >= 1_000:
		return fmt.Sprintf("%dK", *ctx/1_000)
	default:
		return fmt.Sprintf("%d", *ctx)
	}
}

func safeContext(ctx *int64) int64 {
	if ctx == nil {
		return 0
	}
	return *ctx
}

func safeStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func extractProvider(id string) string {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) < 2 {
		return id
	}
	return parts[0]
}

func wrapText(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}

	var b strings.Builder
	words := strings.Fields(text)
	lineLen := 0
	for _, word := range words {
		if lineLen+len(word)+1 > maxLen && lineLen > 0 {
			b.WriteString("\n")
			lineLen = 0
		}
		if lineLen > 0 {
			b.WriteString(" ")
			lineLen++
		}
		b.WriteString(word)
		lineLen += len(word)
	}
	return b.String()
}

func formatUnix(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02")
}
