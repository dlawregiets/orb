package tui

import (
	"context"
	"fmt"
	"sort"
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

type routingProvider struct {
	Slug       string
	Name       string
	Context    int64
	Prompt     float64
	Completion float64
	Endpoints  int

	// Performance statistics are averaged across the provider's endpoints.
	// Latency and throughput are only populated for authenticated requests.
	Uptime5m   *float64
	Uptime30m  *float64
	Uptime1d   *float64
	Latency    *components.PercentileStats
	Throughput *components.PercentileStats
}

type routingState struct {
	modelID     string
	returnView  view
	providers   []routingProvider
	order       []string
	fallbacks   bool
	loading     bool
	err         error
	table       table.Model
	input       textinput.Model
	inputActive bool
	initialized bool
}

type endpointsLoadedMsg struct {
	modelID   string
	providers []routingProvider
	err       error
}

func fetchEndpoints(client *openrouter.OpenRouter, modelID string) tea.Cmd {
	return func() tea.Msg {
		author, slug, ok := strings.Cut(modelID, "/")
		if !ok || author == "" || slug == "" {
			return endpointsLoadedMsg{modelID: modelID, err: fmt.Errorf("invalid model id %q", modelID)}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		res, err := client.Endpoints.List(ctx, author, slug)
		if err != nil {
			return endpointsLoadedMsg{modelID: modelID, err: err}
		}

		var endpoints []components.PublicEndpoint
		if res != nil {
			endpoints = res.Data.Endpoints
		}
		return endpointsLoadedMsg{modelID: modelID, providers: providersFromEndpoints(endpoints)}
	}
}

type providerAccumulator struct {
	provider   routingProvider
	latency    components.PercentileStats
	latencyN   int
	throughput components.PercentileStats
	throughN   int
	uptime5m   float64
	uptime5mN  int
	uptime30m  float64
	uptime30N  int
	uptime1d   float64
	uptime1dN  int
}

// providersFromEndpoints groups endpoints by upstream provider and averages
// their pricing, context, and recent performance statistics.
func providersFromEndpoints(endpoints []components.PublicEndpoint) []routingProvider {
	accumulators := map[string]*providerAccumulator{}
	order := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		slug := strings.SplitN(endpoint.Tag, "/", 2)[0]
		if slug == "" {
			continue
		}

		acc, ok := accumulators[slug]
		if !ok {
			acc = &providerAccumulator{
				provider: routingProvider{
					Slug:       slug,
					Name:       string(endpoint.ProviderName),
					Prompt:     parsePrice(endpoint.Pricing.Prompt),
					Completion: parsePrice(endpoint.Pricing.Completion),
				},
			}
			accumulators[slug] = acc
			order = append(order, slug)
		}
		acc.provider.Endpoints++
		if endpoint.ContextLength > acc.provider.Context {
			acc.provider.Context = endpoint.ContextLength
		}
		if acc.provider.Name == "" {
			acc.provider.Name = string(endpoint.ProviderName)
		}
		if endpoint.LatencyLast30m != nil {
			acc.latency.P50 += endpoint.LatencyLast30m.P50
			acc.latency.P75 += endpoint.LatencyLast30m.P75
			acc.latency.P90 += endpoint.LatencyLast30m.P90
			acc.latency.P99 += endpoint.LatencyLast30m.P99
			acc.latencyN++
		}
		if endpoint.ThroughputLast30m != nil {
			acc.throughput.P50 += endpoint.ThroughputLast30m.P50
			acc.throughput.P75 += endpoint.ThroughputLast30m.P75
			acc.throughput.P90 += endpoint.ThroughputLast30m.P90
			acc.throughput.P99 += endpoint.ThroughputLast30m.P99
			acc.throughN++
		}
		if endpoint.UptimeLast5m != nil {
			acc.uptime5m += *endpoint.UptimeLast5m
			acc.uptime5mN++
		}
		if endpoint.UptimeLast30m != nil {
			acc.uptime30m += *endpoint.UptimeLast30m
			acc.uptime30N++
		}
		if endpoint.UptimeLast1d != nil {
			acc.uptime1d += *endpoint.UptimeLast1d
			acc.uptime1dN++
		}
	}

	out := make([]routingProvider, 0, len(accumulators))
	for _, slug := range order {
		acc := accumulators[slug]
		acc.provider.Uptime5m = averageValue(acc.uptime5m, acc.uptime5mN)
		acc.provider.Uptime30m = averageValue(acc.uptime30m, acc.uptime30N)
		acc.provider.Uptime1d = averageValue(acc.uptime1d, acc.uptime1dN)
		acc.provider.Latency = averagePercentiles(acc.latency, acc.latencyN)
		acc.provider.Throughput = averagePercentiles(acc.throughput, acc.throughN)
		out = append(out, acc.provider)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

func averageValue(total float64, count int) *float64 {
	if count == 0 {
		return nil
	}
	value := total / float64(count)
	return &value
}

func averagePercentiles(total components.PercentileStats, count int) *components.PercentileStats {
	if count == 0 {
		return nil
	}
	stats := components.PercentileStats{
		P50: total.P50 / float64(count),
		P75: total.P75 / float64(count),
		P90: total.P90 / float64(count),
		P99: total.P99 / float64(count),
	}
	return &stats
}

func (m *Model) openRouting(modelID string, returnView view) tea.Cmd {
	if !m.cfg.Allowed(modelID) {
		m.status = "Allow " + modelID + " first with space"
		m.statusIsError = true
		return nil
	}

	routing := m.cfg.Routing(modelID)
	m.routing = routingState{
		modelID:     modelID,
		returnView:  returnView,
		order:       append([]string(nil), routing.Order...),
		fallbacks:   routing.AllowFallbacks,
		loading:     true,
		input:       m.routing.input,
		initialized: true,
	}
	m.routing.input.SetValue("")
	m.routing.input.Blur()
	m.routing.table = newRoutingTable()
	m.resizeRouting(m.height, m.width)
	m.view = viewRouting
	m.status = ""
	m.statusIsError = false
	m.refreshRoutingRows()
	return fetchEndpoints(m.client, modelID)
}

func newRoutingTable() table.Model {
	columns := []table.Column{
		{Title: "Order", Width: 5},
		{Title: "Provider", Width: 22},
		{Title: "Up 30m", Width: 7},
		{Title: "TTFT", Width: 7},
		{Title: "t/s", Width: 6},
		{Title: "Context", Width: 8},
		{Title: "Prompt/M", Width: 10},
		{Title: "Comp/M", Width: 10},
		{Title: "EPs", Width: 4},
	}
	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(10),
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
	return t
}

func (m *Model) resizeRouting(height, width int) {
	if !m.routing.initialized {
		return
	}
	h := height - 14
	if h < 5 {
		h = 5
	}
	m.routing.table.SetHeight(h)

	providerW := 22
	if width >= 140 {
		providerW = 30
	}
	m.routing.table.SetColumns([]table.Column{
		{Title: "Order", Width: 5},
		{Title: "Provider", Width: providerW},
		{Title: "Up 30m", Width: 7},
		{Title: "TTFT", Width: 7},
		{Title: "t/s", Width: 6},
		{Title: "Context", Width: 8},
		{Title: "Prompt/M", Width: 10},
		{Title: "Comp/M", Width: 10},
		{Title: "EPs", Width: 4},
	})
}

func (m *Model) refreshRoutingRows() {
	selected := m.routingSlugAt(m.routing.table.Cursor())
	bySlug := make(map[string]routingProvider, len(m.routing.providers))
	for _, provider := range m.routing.providers {
		bySlug[provider.Slug] = provider
	}

	rows := make([]table.Row, 0, len(m.routing.providers)+len(m.routing.order))
	seen := map[string]bool{}
	for i, slug := range m.routing.order {
		seen[slug] = true
		rows = append(rows, routingRow(providerBySlug(bySlug, slug), fmt.Sprintf("%d", i+1)))
	}
	for _, provider := range m.routing.providers {
		if seen[provider.Slug] {
			continue
		}
		rows = append(rows, routingRow(provider, "-"))
	}

	m.routing.table.SetRows(rows)
	if selected != "" {
		if idx := m.routingIndex(selected); idx >= 0 {
			m.routing.table.SetCursor(idx)
		}
	}
}

func providerBySlug(providers map[string]routingProvider, slug string) routingProvider {
	if provider, ok := providers[slug]; ok {
		return provider
	}
	return routingProvider{Slug: slug}
}

func routingRow(provider routingProvider, order string) table.Row {
	context := "-"
	if provider.Context > 0 {
		context = formatContext(&provider.Context)
	}
	prompt := "-"
	if provider.Prompt > 0 {
		prompt = formatPrice(provider.Prompt)
	}
	completion := "-"
	if provider.Completion > 0 {
		completion = formatPrice(provider.Completion)
	}
	eps := "-"
	if provider.Endpoints > 0 {
		eps = fmt.Sprintf("%d", provider.Endpoints)
	}
	return table.Row{
		order,
		provider.Slug,
		formatPercent(provider.Uptime30m),
		formatLatency(provider.Latency),
		formatThroughput(provider.Throughput),
		context,
		prompt,
		completion,
		eps,
	}
}

func formatPercent(value *float64) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *value)
}

func formatLatency(stats *components.PercentileStats) string {
	if stats == nil || stats.P50 <= 0 {
		return "-"
	}
	return formatMillis(stats.P50)
}

func formatThroughput(stats *components.PercentileStats) string {
	if stats == nil || stats.P50 <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", stats.P50)
}

func (m *Model) routingSlugAt(index int) string {
	rows := m.routing.table.Rows()
	if index < 0 || index >= len(rows) || len(rows[index]) < 2 {
		return ""
	}
	return rows[index][1]
}

func (m *Model) routingIndex(slug string) int {
	for i := range m.routing.table.Rows() {
		if m.routingSlugAt(i) == slug {
			return i
		}
	}
	return -1
}

func (m *Model) routingProviderBySlug(slug string) (routingProvider, bool) {
	for _, provider := range m.routing.providers {
		if provider.Slug == slug {
			return provider, true
		}
	}
	return routingProvider{}, false
}

// routingSelectionDetail summarizes the selected provider, including the
// percentile breakdown behind the table's p50 latency and throughput values.
func (m *Model) routingSelectionDetail() string {
	slug := m.routingSlugAt(m.routing.table.Cursor())
	if slug == "" {
		return ""
	}
	provider, ok := m.routingProviderBySlug(slug)
	if !ok {
		return slug + " · not in the endpoint list"
	}

	label := slug
	if provider.Name != "" && provider.Name != provider.Slug {
		label = provider.Name + " (" + slug + ")"
	}

	stats := make([]string, 0, 3)
	if provider.Uptime5m != nil || provider.Uptime30m != nil || provider.Uptime1d != nil {
		stats = append(stats, fmt.Sprintf("up 5m/30m/1d %s / %s / %s",
			formatPercent(provider.Uptime5m),
			formatPercent(provider.Uptime30m),
			formatPercent(provider.Uptime1d),
		))
	}
	if provider.Latency != nil {
		stats = append(stats, fmt.Sprintf("TTFT p50 %s p90 %s",
			formatMillis(provider.Latency.P50),
			formatMillis(provider.Latency.P90),
		))
	}
	if provider.Throughput != nil {
		stats = append(stats, fmt.Sprintf("t/s p50 %.0f p90 %.0f", provider.Throughput.P50, provider.Throughput.P90))
	}
	if len(stats) == 0 {
		return label + " · no recent performance data"
	}
	return label + " · " + strings.Join(stats, " · ")
}

func formatMillis(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	return fmt.Sprintf("%.0fms", ms)
}

func (m *Model) updateRouting(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.routing.inputActive {
		switch msg.String() {
		case "esc":
			m.routing.input.Blur()
			m.routing.inputActive = false
			m.routing.input.SetValue("")
			return m, nil
		case "enter":
			m.addRoutingProvider(m.routing.input.Value())
			m.routing.input.Blur()
			m.routing.inputActive = false
			m.routing.input.SetValue("")
			return m, nil
		}
		newInput, cmd := m.routing.input.Update(msg)
		m.routing.input = newInput
		return m, cmd
	}

	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit

	case "esc", "q":
		m.view = m.routing.returnView
		return m, nil

	case "s":
		return m, saveConfig(m.cfg)

	case " ":
		slug := m.routingSlugAt(m.routing.table.Cursor())
		if slug == "" {
			return m, nil
		}
		if index := indexOf(m.routing.order, slug); index >= 0 {
			m.routing.order = append(m.routing.order[:index], m.routing.order[index+1:]...)
			m.status = "Removed " + slug + " from the routing order"
		} else {
			m.routing.order = append(m.routing.order, slug)
			m.status = "Added " + slug + " to the routing order"
		}
		m.statusIsError = false
		m.applyRouting()
		m.refreshRoutingRows()
		return m, nil

	case "a":
		m.routing.fallbacks = !m.routing.fallbacks
		m.status = "Fallbacks disabled"
		if m.routing.fallbacks {
			m.status = "Fallbacks enabled"
		}
		m.statusIsError = false
		m.applyRouting()
		return m, nil

	case "J", "K":
		slug := m.routingSlugAt(m.routing.table.Cursor())
		index := indexOf(m.routing.order, slug)
		if index < 0 {
			m.status = "Add " + slug + " to the order first with space"
			m.statusIsError = true
			return m, nil
		}
		delta := 1
		if msg.String() == "K" {
			delta = -1
		}
		next := index + delta
		if next < 0 || next >= len(m.routing.order) {
			return m, nil
		}
		m.routing.order[index], m.routing.order[next] = m.routing.order[next], m.routing.order[index]
		m.applyRouting()
		m.refreshRoutingRows()
		m.routing.table.SetCursor(next)
		m.status = "Moved " + slug + " to position " + fmt.Sprintf("%d", next+1)
		m.statusIsError = false
		return m, nil

	case "i":
		m.routing.input.Focus()
		m.routing.inputActive = true
		return m, textinput.Blink
	}

	var cmd tea.Cmd
	m.routing.table, cmd = m.routing.table.Update(msg)
	return m, cmd
}

func (m *Model) addRoutingProvider(value string) {
	slug := strings.TrimSpace(value)
	if slug == "" {
		return
	}
	for _, r := range slug {
		if !isProviderSlugChar(r) {
			m.status = "Invalid provider slug: " + slug
			m.statusIsError = true
			return
		}
	}
	if index := indexOf(m.routing.order, slug); index < 0 {
		m.routing.order = append(m.routing.order, slug)
		m.applyRouting()
		m.refreshRoutingRows()
		if idx := m.routingIndex(slug); idx >= 0 {
			m.routing.table.SetCursor(idx)
		}
	}
	m.status = "Added " + slug + " to the routing order"
	m.statusIsError = false
}

func (m *Model) applyRouting() {
	m.cfg.SetRouting(m.routing.modelID, opencode.Routing{
		Order:          append([]string(nil), m.routing.order...),
		AllowFallbacks: m.routing.fallbacks,
		Configured:     true,
	})
	m.allowed = m.cfg.AllowedIDs()
}

func (m *Model) handleEndpoints(msg endpointsLoadedMsg) {
	if !m.routing.initialized || msg.modelID != m.routing.modelID {
		return
	}
	m.routing.providers = msg.providers
	m.routing.loading = false
	m.routing.err = msg.err
	m.refreshRoutingRows()
}

func (m *Model) routingView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(" Routing: " + m.routing.modelID + " "))
	fmt.Fprint(&b, "\n\n")

	fallbacks := "on"
	if !m.routing.fallbacks {
		fallbacks = "off"
	}
	fmt.Fprintf(&b, " %s %s  [%s]   %s %d  %s\n",
		headerStyle.Render("Fallbacks:"), filterStyle.Render(fallbacks), keyStyle.Render("a"),
		headerStyle.Render("Ordered:"), len(m.routing.order),
		helpStyle.Render("(empty order = OpenRouter automatic)"),
	)

	if m.routing.loading {
		fmt.Fprint(&b, "\n")
		b.WriteString(helpStyle.Render(" Fetching endpoints from OpenRouter..."))
		fmt.Fprint(&b, "\n")
	} else if m.routing.err != nil {
		fmt.Fprint(&b, "\n")
		b.WriteString(errorStyle.Render(" Endpoints: " + m.routing.err.Error()))
		fmt.Fprint(&b, "\n")
	} else if len(m.routing.providers) == 0 {
		fmt.Fprint(&b, "\n")
		b.WriteString(helpStyle.Render(" No endpoint list is available for this model. Add provider slugs manually with i."))
		fmt.Fprint(&b, "\n")
	}

	fmt.Fprint(&b, "\n")
	b.WriteString(baseStyle.Render(m.routing.table.View()))
	fmt.Fprint(&b, "\n")

	if detail := m.routingSelectionDetail(); detail != "" {
		for i, line := range strings.Split(wrapText(detail, m.width-8), "\n") {
			prefix := " "
			if i > 0 {
				prefix = "   "
			}
			b.WriteString(helpStyle.Render(prefix + line))
			fmt.Fprint(&b, "\n")
		}
	}
	if !m.authenticated && !m.routing.loading && m.routing.err == nil && len(m.routing.providers) > 0 {
		b.WriteString(helpStyle.Render(" TTFT and t/s require OPENROUTER_API_KEY."))
		fmt.Fprint(&b, "\n")
	}

	if m.routing.inputActive {
		fmt.Fprintf(&b, " %s %s\n", filterStyle.Render("Add provider:"), m.routing.input.View())
	}

	b.WriteString(helpStyle.Render(" space include/exclude  J/K reorder  a fallbacks  i add slug  s save  esc/q back"))
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

func indexOf(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}

func isProviderSlugChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '-', r == '_', r == '.':
		return true
	default:
		return false
	}
}
