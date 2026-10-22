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
	// Priced reports whether the catalog supplied pricing for this provider.
	// Completion may legitimately be zero, so it cannot be used as the marker.
	Priced bool
	// Discount is OpenRouter's fractional endpoint discount: the effective
	// price is the listed price multiplied by (1 - Discount).
	Discount  *float64
	Endpoints int

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
	sortBy      routingSortKey
	sortDesc    bool
	loading     bool
	err         error
	table       routingTable
	input       textinput.Model
	inputActive bool
	initialized bool
}

type routingSortKey int

const (
	routingSortOrder routingSortKey = iota
	routingSortName
	routingSortThroughput
	routingSortCost
	routingSortValue
)

type routingColumn struct {
	Title string
	Width int
}

// routingTable keeps the cursor and row state for the routing view. The
// bubbles table renders cells with runewidth.Truncate, which counts ANSI
// escapes as display width, so per-cell heatmap colors would corrupt its
// layout. This small table leaves rendering to the view, which can style and
// truncate each cell itself.
type routingTable struct {
	columns []routingColumn
	rows    [][]string
	cursor  int
	height  int
}

const (
	routingThroughputColumn = 4
	routingValueColumn      = 5
	routingCostColumn       = 8
)

// routingHeat holds 0..1 scores, where 1 is the fastest or cheapest provider
// on the page. Value blends the two and is only set when both are known.
type routingHeat struct {
	SpeedScore float64
	CostScore  float64
	Value      float64
	HasSpeed   bool
	HasCost    bool
	HasValue   bool
}

// routingEntry is one visible provider row. order is the provider's 1-based
// position in the routing order, or 0 when it is not included.
type routingEntry struct {
	provider routingProvider
	order    int
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
					Priced:     true,
					Discount:   endpoint.Pricing.Discount,
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
	sortBy, sortDesc := m.routing.sortBy, m.routing.sortDesc
	m.routing = routingState{
		modelID:     modelID,
		returnView:  returnView,
		order:       append([]string(nil), routing.Order...),
		fallbacks:   routing.AllowFallbacks,
		sortBy:      sortBy,
		sortDesc:    sortDesc,
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

func newRoutingTable() routingTable {
	t := routingTable{height: 8}
	t.SetColumns(routingColumns(22))
	return t
}

func routingColumns(providerWidth int) []routingColumn {
	return []routingColumn{
		{Title: "Order", Width: 5},
		{Title: "Provider", Width: providerWidth},
		{Title: "Up 30m", Width: 7},
		{Title: "TTFT", Width: 7},
		{Title: "t/s", Width: 6},
		{Title: "Value", Width: 6},
		{Title: "Context", Width: 8},
		{Title: "Prompt/M", Width: 10},
		{Title: "Comp/M", Width: 10},
		{Title: "EPs", Width: 4},
	}
}

func (t *routingTable) SetColumns(columns []routingColumn) {
	t.columns = columns
}

func (t *routingTable) Columns() []routingColumn {
	return t.columns
}

func (t *routingTable) SetRows(rows [][]string) {
	t.rows = rows
	if t.cursor > len(rows)-1 {
		t.cursor = max(len(rows)-1, 0)
	}
}

func (t *routingTable) Rows() [][]string {
	return t.rows
}

// SetHeight reserves two lines for the header and its separator.
func (t *routingTable) SetHeight(height int) {
	t.height = max(height-2, 1)
}

func (t *routingTable) Height() int {
	return t.height
}

func (t *routingTable) Cursor() int {
	return t.cursor
}

func (t *routingTable) SetCursor(index int) {
	if len(t.rows) == 0 {
		t.cursor = 0
		return
	}
	t.cursor = min(max(index, 0), len(t.rows)-1)
}

func (t *routingTable) move(delta int) {
	t.SetCursor(t.cursor + delta)
}

func (t routingTable) Update(msg tea.KeyMsg) (routingTable, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		t.move(-1)
	case "down", "j":
		t.move(1)
	case "pgup", "b", "ctrl+u":
		t.move(-t.height)
	case "pgdown", "f", "ctrl+d":
		t.move(t.height)
	case "home", "g":
		t.SetCursor(0)
	case "end", "G":
		t.SetCursor(len(t.rows) - 1)
	}
	return t, nil
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
	m.routing.table.SetColumns(routingColumns(providerW))
}

func (m *Model) refreshRoutingRows() {
	selected := m.routingSlugAt(m.routing.table.Cursor())
	heat := m.routingHeatmap()
	entries := m.routingEntries(heat)

	rows := make([][]string, 0, len(entries))
	for _, entry := range entries {
		order := "-"
		if entry.order > 0 {
			order = strconv.Itoa(entry.order)
		}
		rows = append(rows, routingRowWithValue(entry.provider, order, heat[entry.provider.Slug]))
	}

	m.routing.table.SetRows(rows)
	if selected != "" {
		if idx := m.routingIndex(selected); idx >= 0 {
			m.routing.table.SetCursor(idx)
		}
	}
}

// routingEntries builds the rows in routing order, then applies the active
// sort. Providers missing the sorted metric go last so they never displace
// the values being compared.
func (m *Model) routingEntries(heat map[string]routingHeat) []routingEntry {
	bySlug := make(map[string]routingProvider, len(m.routing.providers))
	for _, provider := range m.routing.providers {
		bySlug[provider.Slug] = provider
	}

	entries := make([]routingEntry, 0, len(m.routing.providers)+len(m.routing.order))
	seen := map[string]bool{}
	for i, slug := range m.routing.order {
		seen[slug] = true
		entries = append(entries, routingEntry{provider: providerBySlug(bySlug, slug), order: i + 1})
	}
	for _, provider := range m.routing.providers {
		if seen[provider.Slug] {
			continue
		}
		entries = append(entries, routingEntry{provider: provider})
	}

	if m.routing.sortBy == routingSortOrder {
		return entries
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return m.routingEntryLess(entries[i], entries[j], heat)
	})
	return entries
}

func (m *Model) routingEntryLess(a, b routingEntry, heat map[string]routingHeat) bool {
	nameA := strings.ToLower(routingProviderName(a.provider))
	nameB := strings.ToLower(routingProviderName(b.provider))

	if m.routing.sortBy == routingSortName {
		if nameA == nameB {
			return a.provider.Slug < b.provider.Slug
		}
		if m.routing.sortDesc {
			return nameA > nameB
		}
		return nameA < nameB
	}

	valueA, okA := routingSortMetric(a.provider, m.routing.sortBy, heat)
	valueB, okB := routingSortMetric(b.provider, m.routing.sortBy, heat)
	if okA != okB {
		return okA
	}
	if !okA || valueA == valueB {
		return nameA < nameB
	}
	if m.routing.sortDesc {
		return valueA > valueB
	}
	return valueA < valueB
}

func routingProviderName(provider routingProvider) string {
	if provider.Slug != "" {
		return provider.Slug
	}
	return provider.Name
}

func routingSortMetric(provider routingProvider, key routingSortKey, heat map[string]routingHeat) (float64, bool) {
	switch key {
	case routingSortThroughput:
		if provider.Throughput != nil && provider.Throughput.P50 > 0 {
			return provider.Throughput.P50, true
		}
	case routingSortCost:
		if provider.Priced {
			return provider.effectiveCompletion() * 1_000_000, true
		}
	case routingSortValue:
		if h := heat[provider.Slug]; h.HasValue {
			return h.Value, true
		}
	}
	return 0, false
}

func (m *Model) setRoutingSort(key routingSortKey) {
	if key != routingSortName && !m.hasRoutingSortMetric(key) {
		switch key {
		case routingSortThroughput:
			m.status = "t/s sorting requires OPENROUTER_API_KEY"
		case routingSortValue:
			m.status = "Value sorting requires t/s data; set OPENROUTER_API_KEY"
		default:
			m.status = "No pricing data to sort by"
		}
		m.statusIsError = true
		return
	}

	if m.routing.sortBy == key {
		m.routing.sortDesc = !m.routing.sortDesc
	} else {
		m.routing.sortBy = key
		// Fastest, cheapest and best value read best-first; names read A-Z.
		m.routing.sortDesc = key == routingSortThroughput || key == routingSortValue
	}
	m.refreshRoutingRows()
	m.status = m.routingSortStatus()
	m.statusIsError = false
}

func (m *Model) hasRoutingSortMetric(key routingSortKey) bool {
	heat := m.routingHeatmap()
	for _, provider := range m.routing.providers {
		if _, ok := routingSortMetric(provider, key, heat); ok {
			return true
		}
	}
	return false
}

func (m *Model) routingSortStatus() string {
	desc := m.routing.sortDesc
	switch m.routing.sortBy {
	case routingSortName:
		if desc {
			return "Sorted by name (Z→A)"
		}
		return "Sorted by name (A→Z)"
	case routingSortThroughput:
		if desc {
			return "Sorted by t/s (fastest first)"
		}
		return "Sorted by t/s (slowest first)"
	case routingSortCost:
		if desc {
			return "Sorted by cost (most expensive first)"
		}
		return "Sorted by cost (cheapest first)"
	case routingSortValue:
		if desc {
			return "Sorted by value (best first)"
		}
		return "Sorted by value (worst first)"
	default:
		return "Sorted by routing order"
	}
}

func (m *Model) clearRoutingSort() {
	if m.routing.sortBy == routingSortOrder {
		return
	}
	m.routing.sortBy = routingSortOrder
	m.routing.sortDesc = false
	m.refreshRoutingRows()
	m.status = "Sorting cleared; showing routing order"
	m.statusIsError = false
}

func (m *Model) routingSortLabel() string {
	var label string
	switch m.routing.sortBy {
	case routingSortName:
		label = "name"
	case routingSortThroughput:
		label = "t/s"
	case routingSortCost:
		label = "cost"
	case routingSortValue:
		label = "value"
	default:
		return "order"
	}
	if m.routing.sortDesc {
		return label + "↓"
	}
	return label + "↑"
}

func (m *Model) routingSortColumn() int {
	switch m.routing.sortBy {
	case routingSortName:
		return 1
	case routingSortThroughput:
		return routingThroughputColumn
	case routingSortCost:
		return routingCostColumn
	case routingSortValue:
		return routingValueColumn
	default:
		return -1
	}
}

func providerBySlug(providers map[string]routingProvider, slug string) routingProvider {
	if provider, ok := providers[slug]; ok {
		return provider
	}
	return routingProvider{Slug: slug}
}

func routingRow(provider routingProvider, order string) []string {
	context := "-"
	if provider.Context > 0 {
		context = formatContext(&provider.Context)
	}
	prompt := "-"
	completion := "-"
	if provider.Priced {
		prompt = formatPrice(provider.effectivePrompt())
		completion = formatPrice(provider.effectiveCompletion())
	}
	eps := "-"
	if provider.Endpoints > 0 {
		eps = fmt.Sprintf("%d", provider.Endpoints)
	}
	return []string{
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

// routingRowWithValue inserts the combined heatmap score between the
// throughput and context columns. routingRow stays value-free because the
// score depends on the other providers in the page.
func routingRowWithValue(provider routingProvider, order string, heat routingHeat) []string {
	row := routingRow(provider, order)
	value := "-"
	if heat.HasValue {
		value = fmt.Sprintf("%d", int(heat.Value*100+0.5))
	}
	out := make([]string, 0, len(row)+1)
	out = append(out, row[:routingValueColumn]...)
	out = append(out, value)
	out = append(out, row[routingValueColumn:]...)
	return out
}

func (p routingProvider) discountMultiplier() float64 {
	if p.Discount == nil {
		return 1
	}
	return 1 - min(max(*p.Discount, 0), 1)
}

func (p routingProvider) effectivePrompt() float64 {
	return p.Prompt * p.discountMultiplier()
}

func (p routingProvider) effectiveCompletion() float64 {
	return p.Completion * p.discountMultiplier()
}

// routingHeatmap ranks providers against each other. Throughput ranks from
// slow to fast and effective output price ranks from expensive to cheap, so a
// score of 1 always means the best provider in that dimension on this page.
// The combined Value score needs both metrics; providers with only one are
// still colored in the column that is known.
func (m *Model) routingHeatmap() map[string]routingHeat {
	type entry struct {
		slug     string
		speed    float64
		cost     float64
		hasSpeed bool
		hasCost  bool
	}

	entries := make([]entry, 0, len(m.routing.providers))
	speeds := make([]float64, 0, len(m.routing.providers))
	costs := make([]float64, 0, len(m.routing.providers))
	for _, provider := range m.routing.providers {
		item := entry{slug: provider.Slug}
		if provider.Throughput != nil && provider.Throughput.P50 > 0 {
			item.hasSpeed = true
			item.speed = provider.Throughput.P50
			speeds = append(speeds, item.speed)
		}
		if provider.Priced {
			item.hasCost = true
			item.cost = provider.effectiveCompletion() * 1_000_000
			costs = append(costs, item.cost)
		}
		entries = append(entries, item)
	}

	speedScores := rankScores(speeds, false)
	costScores := rankScores(costs, true)
	heat := make(map[string]routingHeat, len(entries))
	speedIdx, costIdx := 0, 0
	for _, item := range entries {
		h := routingHeat{}
		if item.hasSpeed {
			h.HasSpeed = true
			h.SpeedScore = speedScores[speedIdx]
			speedIdx++
		}
		if item.hasCost {
			h.HasCost = true
			h.CostScore = costScores[costIdx]
			costIdx++
		}
		if h.HasSpeed && h.HasCost {
			h.HasValue = true
			h.Value = (h.SpeedScore + h.CostScore) / 2
		}
		heat[item.slug] = h
	}
	return heat
}

// rankScores maps values to 0..1 ranks, using the average rank for ties. A
// single value is neutral because there is nothing to compare it with.
func rankScores(values []float64, lowerBetter bool) []float64 {
	scores := make([]float64, len(values))
	if len(values) == 0 {
		return scores
	}
	if len(values) == 1 {
		scores[0] = 0.5
		return scores
	}

	type item struct {
		value float64
		index int
	}
	sorted := make([]item, len(values))
	for i, value := range values {
		sorted[i] = item{value: value, index: i}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].value < sorted[j].value })

	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1].value == sorted[i].value {
			j++
		}
		rank := (float64(i) + float64(j)) / 2 / float64(len(sorted)-1)
		score := rank
		if lowerBetter {
			score = 1 - rank
		}
		for k := i; k <= j; k++ {
			scores[sorted[k].index] = score
		}
		i = j + 1
	}
	return scores
}

var (
	routingHeaderStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#A78BFA"))
	routingSeparatorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#3730A3"))
	routingSelectedColor  = lipgloss.Color("#4C1D95")
	routingValueTextColor = lipgloss.Color("#111827")
)

func (m *Model) routingTableView() string {
	columns := m.routing.table.Columns()
	heat := m.routingHeatmap()

	var b strings.Builder
	sortColumn := m.routingSortColumn()
	for index, column := range columns {
		title := column.Title
		if index == sortColumn {
			if m.routing.sortDesc {
				title += "↓"
			} else {
				title += "↑"
			}
		}
		b.WriteString(routingHeaderStyle.Render(" " + fitRoutingCell(title, column.Width) + " "))
	}
	b.WriteByte('\n')
	totalWidth := 0
	for _, column := range columns {
		totalWidth += column.Width + 2
	}
	b.WriteString(routingSeparatorStyle.Render(strings.Repeat("─", totalWidth)))

	rows := m.routing.table.Rows()
	if len(rows) == 0 {
		return b.String()
	}

	height := m.routing.table.Height()
	start := 0
	if len(rows) > height {
		start = min(max(m.routing.table.Cursor()-height/2, 0), len(rows)-height)
	}
	end := min(start+height, len(rows))
	for i := start; i < end; i++ {
		b.WriteByte('\n')
		slug := ""
		if len(rows[i]) > 1 {
			slug = rows[i][1]
		}
		selected := i == m.routing.table.Cursor()
		b.WriteString(renderRoutingRow(columns, rows[i], heat[slug], selected))
	}
	return b.String()
}

func renderRoutingRow(columns []routingColumn, row []string, heat routingHeat, selected bool) string {
	var b strings.Builder
	for i, column := range columns {
		text := ""
		if i < len(row) {
			text = row[i]
		}

		var fg, bg lipgloss.TerminalColor
		bold := false
		switch {
		case i == routingValueColumn && heat.HasValue:
			fg = routingValueTextColor
			bg = heatColor(heat.Value)
			bold = true
		case selected:
			fg = lipgloss.Color("#FFFFFF")
			bg = routingSelectedColor
			bold = true
		case i == routingThroughputColumn && heat.HasSpeed:
			fg = heatColor(heat.SpeedScore)
		case i == routingCostColumn && heat.HasCost:
			fg = heatColor(heat.CostScore)
		}

		style := lipgloss.NewStyle()
		if fg != nil {
			style = style.Foreground(fg)
		}
		if bg != nil {
			style = style.Background(bg)
		}
		if bold {
			style = style.Bold(true)
		}
		b.WriteString(style.Render(" " + fitRoutingCell(text, column.Width) + " "))
	}
	return b.String()
}

// fitRoutingCell truncates and pads a plain cell to its display width. Cells
// contain no ANSI escapes, so rune count is a close enough width measure.
func fitRoutingCell(text string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) > width {
		if width == 1 {
			return "…"
		}
		return string(runes[:width-1]) + "…"
	}
	return text + strings.Repeat(" ", width-len(runes))
}

// heatColor maps a 0..1 score to a red-yellow-green gradient.
func heatColor(score float64) lipgloss.Color {
	score = min(max(score, 0), 1)
	from, to := "#EF4444", "#EAB308"
	progress := score / 0.5
	if score >= 0.5 {
		from, to = to, "#22C55E"
		progress = (score - 0.5) / 0.5
	}
	return lipgloss.Color(blendHex(from, to, progress))
}

func blendHex(from, to string, progress float64) string {
	fromR, fromG, fromB := parseHex(from)
	toR, toG, toB := parseHex(to)
	lerp := func(a, b int) int { return int(float64(a) + float64(b-a)*progress + 0.5) }
	return fmt.Sprintf("#%02X%02X%02X", lerp(fromR, toR), lerp(fromG, toG), lerp(fromB, toB))
}

func parseHex(color string) (int, int, int) {
	value, _ := strconv.ParseUint(strings.TrimPrefix(color, "#"), 16, 24)
	return int(value>>16) & 0xFF, int(value>>8) & 0xFF, int(value) & 0xFF
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

	stats := make([]string, 0, 5)
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
	heat := m.routingHeatmap()[provider.Slug]
	if heat.HasValue {
		stats = append(stats, fmt.Sprintf("value %.0f/100", heat.Value*100))
	}
	if provider.Priced {
		out := formatPrice(provider.effectiveCompletion())
		if provider.Discount != nil && *provider.Discount > 0 {
			out += fmt.Sprintf(" (%d%% off)", int(*provider.Discount*100+0.5))
		}
		stats = append(stats, "out "+out+"/M")
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
		if m.routing.sortBy != routingSortOrder {
			// Reordering follows the visible routing order, so drop the sort
			// first instead of moving a row that is not where it appears.
			m.routing.sortBy = routingSortOrder
			m.routing.sortDesc = false
			m.refreshRoutingRows()
		}
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

	case "n":
		m.setRoutingSort(routingSortName)
		return m, nil

	case "t":
		m.setRoutingSort(routingSortThroughput)
		return m, nil

	case "c":
		m.setRoutingSort(routingSortCost)
		return m, nil

	case "v":
		m.setRoutingSort(routingSortValue)
		return m, nil

	case "u":
		m.clearRoutingSort()
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
	fmt.Fprintf(&b, " %s %s  [%s]   %s %d   %s %s   %s\n",
		headerStyle.Render("Fallbacks:"), filterStyle.Render(fallbacks), keyStyle.Render("a"),
		headerStyle.Render("Ordered:"), len(m.routing.order),
		headerStyle.Render("Sort:"), m.routingSortLabel(),
		helpStyle.Render("(empty = automatic)"),
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
	b.WriteString(baseStyle.Render(m.routingTableView()))
	fmt.Fprint(&b, "\n")

	if !m.routing.loading && m.routing.err == nil && len(m.routing.providers) > 0 {
		b.WriteString(helpStyle.Render(" Heat: green = best value on this page, red = worst. Value blends p50 t/s with effective $/M output."))
		fmt.Fprint(&b, "\n")
	}

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

	b.WriteString(helpStyle.Render(" space include/exclude  J/K reorder  i add slug  n/t/c/v sort  u order  s save  esc/q back"))
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
