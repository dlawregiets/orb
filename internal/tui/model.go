// Package tui provides a terminal UI for browsing OpenRouter models and
// curating the models allowed by an OpenCode configuration.
package tui

import (
	"context"
	"fmt"
	"math"
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
	viewAgents
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

// marqueeTickMsg advances the horizontal scroll of an overlong selected model
// name in the list view.
type marqueeTickMsg struct{}

// marqueeInterval is the delay between marquee steps.
const marqueeInterval = 120 * time.Millisecond

// marqueeGap is the number of blank runes inserted between repeats of an
// overlong model name while it scrolls.
const marqueeGap = 3

// fetchModels returns a tea.Cmd that loads the OpenRouter model catalog as a
// modelsLoadedMsg, or errMsg on failure.
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
	agents  agentState

	status        string
	statusIsError bool
	confirmQuit   bool

	// detailOffset is the first visible line of the scrollable detail body.
	detailOffset int

	// The list view scrolls the selected model name horizontally when it does
	// not fit the Model column. modelColWidth tracks that column's width.
	modelColWidth int
	marqueeOffset int
	marqueeActive bool
	marqueeRow    int
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
		agents:        newAgentState(),
		modelColWidth: 60,
		marqueeRow:    -1,
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

		// Leave room for the header, filters, help and status lines.
		h := msg.Height - 12
		h = int(math.Max(float64(h), 5))
		m.table.SetHeight(h)

		// The model column gets 40% of the width; the remaining width is
		// split evenly across the five data columns, both with floors so the
		// table stays readable on narrow terminals.
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
		m.modelColWidth = modelW
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
		return m, m.maybeStartMarquee()

	case marqueeTickMsg:
		if m.view != viewList || !m.marqueeNeeded() {
			m.stopMarquee()
			return m, nil
		}
		m.advanceMarquee()
		return m, tea.Tick(marqueeInterval, func(time.Time) tea.Msg {
			return marqueeTickMsg{}
		})

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
		// The routing view and the detail view each capture most keys, so
		// they are handled by dedicated updaters below.
		if m.view == viewRouting {
			return m.updateRouting(msg)
		}

		if m.view == viewAgents {
			return m.updateAgents(msg)
		}

		if m.view == viewDetail {
			switch msg.String() {
			case "esc", "q", "enter":
				m.view = viewList
				m.detailOffset = 0
				return m, m.maybeStartMarquee()
			case " ":
				m.toggleSelectedAllowed()
				return m, nil
			case "p":
				if id := m.currentModelID(); id != "" {
					return m, m.openRouting(id, viewDetail)
				}
				return m, nil
			case "a":
				if id := m.currentModelID(); id != "" {
					return m, m.openAgents(id, viewDetail)
				}
				return m, nil
			case "s":
				return m, saveConfig(m.cfg)
			case "up", "k":
				m.scrollDetail(-1)
			case "down", "j":
				m.scrollDetail(1)
			case "pgup", "b", "ctrl+u":
				m.scrollDetail(-m.detailVisibleHeight())
			case "pgdown", "f", "ctrl+d":
				m.scrollDetail(m.detailVisibleHeight())
			case "home", "g":
				m.detailOffset = 0
			case "end", "G":
				m.detailOffset = m.detailMaxOffset()
			}
			return m, nil
		}

		// While an input is focused it swallows keys; filters re-apply on
		// every character so the list updates live.
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

		case "a":
			if id := m.currentModelID(); id != "" {
				return m, m.openAgents(id, viewList)
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
			if idx := m.table.Cursor(); idx >= 0 && idx < len(m.filtered) {
				m.selectedIdx = idx
				m.view = viewDetail
				m.detailOffset = 0
				m.stopMarquee()
			}
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	m.resetMarqueeForSelection()
	return m, tea.Batch(cmd, m.maybeStartMarquee())
}

// applyFilters recomputes m.filtered from the full model catalog according
// to the search box, the free/allowed/modality toggles and the knowledge
// cutoff year, then rebuilds the table rows sorted by context length.
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

		// Cutoff filtering: keep models whose knowledge cutoff is at or
		// after the entered year. Models without a cutoff are dropped.
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
	m.marqueeRow = -1
	m.marqueeOffset = 0
}

// marqueeNeeded reports whether the selected model name overflows the Model
// column and therefore needs to scroll.
func (m *Model) marqueeNeeded() bool {
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.filtered) || m.modelColWidth <= 0 {
		return false
	}
	return len([]rune(m.filtered[idx].ID)) > m.modelColWidth
}

// maybeStartMarquee schedules the first marquee tick when the selected name
// does not fit. It does nothing when an animation is already running.
func (m *Model) maybeStartMarquee() tea.Cmd {
	if m.marqueeActive || m.view != viewList || !m.marqueeNeeded() {
		return nil
	}
	m.marqueeActive = true
	m.marqueeRow = -1
	m.marqueeOffset = 0
	return tea.Tick(marqueeInterval, func(time.Time) tea.Msg {
		return marqueeTickMsg{}
	})
}

// advanceMarquee shifts the visible window one rune and repaints the row.
func (m *Model) advanceMarquee() {
	idx := m.table.Cursor()
	if m.marqueeRow != idx {
		m.restoreMarqueeCell()
		m.marqueeRow = idx
		m.marqueeOffset = 0
	} else {
		m.marqueeOffset++
	}
	m.applyMarqueeCell()
}

// resetMarqueeForSelection restores the old row as soon as the cursor moves.
func (m *Model) resetMarqueeForSelection() {
	if m.marqueeRow < 0 || m.marqueeRow == m.table.Cursor() {
		return
	}
	m.restoreMarqueeCell()
	m.marqueeRow = -1
	m.marqueeOffset = 0
}

// stopMarquee restores the full name and halts the animation.
func (m *Model) stopMarquee() {
	m.restoreMarqueeCell()
	m.marqueeActive = false
	m.marqueeRow = -1
	m.marqueeOffset = 0
}

// restoreMarqueeCell puts the row's full name back into the table.
func (m *Model) restoreMarqueeCell() {
	rows := m.table.Rows()
	idx := m.marqueeRow
	if idx < 0 || idx >= len(rows) || idx >= len(m.filtered) || len(rows[idx]) == 0 {
		return
	}
	rows[idx][0] = m.filtered[idx].ID
	m.table.SetRows(rows)
}

// applyMarqueeCell replaces the selected row's name with a shifted window.
func (m *Model) applyMarqueeCell() {
	rows := m.table.Rows()
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(rows) || idx >= len(m.filtered) || len(rows[idx]) == 0 {
		return
	}
	rows[idx][0] = marqueeWindow(m.filtered[idx].ID, m.modelColWidth, m.marqueeOffset)
	m.table.SetRows(rows)
}

// marqueeWindow returns width runes from the name, looping through a short
// gap so the text scrolls continuously.
func marqueeWindow(name string, width, offset int) string {
	runes := []rune(name)
	if width <= 0 || len(runes) == 0 {
		return name
	}
	cycle := len(runes) + marqueeGap
	offset %= cycle
	if offset < 0 {
		offset += cycle
	}
	window := make([]rune, 0, width)
	for pos := offset; len(window) < width; pos++ {
		if pos%cycle < len(runes) {
			window = append(window, runes[pos%cycle])
		} else {
			window = append(window, ' ')
		}
	}
	return string(window)
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

	if m.view == viewAgents {
		return m.agentView()
	}

	if m.view == viewDetail {
		return m.detailView()
	}

	return m.listView()
}

// listView renders the filter row and the main model table.
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
	b.WriteString(helpStyle.Render(" ↑↓ nav  enter detail  space allow  a agents  p routing  s save  q quit  / search  f free  o allowed  m modality  y cutoff  r refresh"))
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

// detailView renders the selected model's metadata. The body scrolls when it
// is taller than the terminal. It falls back to the list view when the
// selection is no longer valid (e.g. after refiltering).
func (m *Model) detailView() string {
	if m.selectedIdx < 0 || m.selectedIdx >= len(m.filtered) {
		m.view = viewList
		return m.listView()
	}

	model := m.filtered[m.selectedIdx]
	lines := m.detailLines(model, m.detailValueWidth())
	visible := m.detailVisibleHeight()
	maxOffset := max(len(lines)-visible, 0)
	m.detailOffset = min(max(m.detailOffset, 0), maxOffset)
	end := min(m.detailOffset+visible, len(lines))

	var b strings.Builder
	b.WriteString(titleStyle.Render(" " + model.Name + " "))
	fmt.Fprint(&b, "\n\n")
	for _, line := range lines[m.detailOffset:end] {
		b.WriteString(line)
		fmt.Fprint(&b, "\n")
	}

	if maxOffset > 0 {
		percent := int(float64(m.detailOffset)/float64(maxOffset)*100 + 0.5)
		b.WriteString(helpStyle.Render(fmt.Sprintf(" %d%%  ↑↓ scroll", percent)))
		fmt.Fprint(&b, "\n")
	}

	b.WriteString(helpStyle.Render(" space allow  a agents  p routing  s save  esc/q back"))
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

// detailValueWidth is the wrap width for detail field values.
func (m *Model) detailValueWidth() int {
	valWidth := m.width - 26
	if valWidth < 30 {
		valWidth = 30
	}
	return valWidth
}

// detailVisibleHeight is the number of detail body lines that fit between the
// title and the footer.
func (m *Model) detailVisibleHeight() int {
	h := m.height - 9
	if h < 3 {
		h = 3
	}
	return h
}

// detailMaxOffset is the largest scroll offset for the selected model.
func (m *Model) detailMaxOffset() int {
	if m.selectedIdx < 0 || m.selectedIdx >= len(m.filtered) {
		return 0
	}
	lines := m.detailLines(m.filtered[m.selectedIdx], m.detailValueWidth())
	return max(len(lines)-m.detailVisibleHeight(), 0)
}

// scrollDetail moves the detail body by delta lines, clamped to its bounds.
func (m *Model) scrollDetail(delta int) {
	m.detailOffset = min(max(m.detailOffset+delta, 0), m.detailMaxOffset())
}

// detailLines renders the scrollable body of the detail view as text lines.
func (m *Model) detailLines(model components.Model, valWidth int) []string {
	var b strings.Builder

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

	if agents := m.cfg.AgentsUsing(model.ID); len(agents) > 0 {
		wrapField(&b, "Agents", strings.Join(agents, ", "), valWidth)
	} else {
		wrapField(&b, "Agents", "none (a to assign)", valWidth)
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

	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

// checkbox renders a "[x] Label  [key]" toggle for the filter row.
func checkbox(label string, checked bool, key string) string {
	if checked {
		return fmt.Sprintf("[x] %s  [%s]", label, keyStyle.Render(key))
	}
	return fmt.Sprintf("[ ] %s  [%s]", label, keyStyle.Render(key))
}

// wrapField writes one "Label: value" line, wrapping long values onto
// continuation lines aligned under the label.
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

// parsePrice parses OpenRouter's per-token price string; unparseable or
// empty prices count as free.
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

// formatPrice converts a per-token price to per-million display with
// enough precision for cheap models, or "Free" for zero.
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

// formatContext abbreviates a context length as N, NK or N.NM.
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

// safeContext dereferences an optional context length, treating nil as 0
// so models without one sort last.
func safeContext(ctx *int64) int64 {
	if ctx == nil {
		return 0
	}
	return *ctx
}

// safeStr dereferences an optional string, treating nil as empty.
func safeStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// extractProvider returns the slug before the "/" in a model ID, or the
// whole ID when there is no slash.
func extractProvider(id string) string {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) < 2 {
		return id
	}
	return parts[0]
}

// wrapText wraps text at word boundaries so no line exceeds maxLen
// characters. Overlong words are not broken.
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

// formatUnix renders a timestamp as a plain date.
func formatUnix(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02")
}
