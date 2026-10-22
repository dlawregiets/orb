package tui

import (
	"strings"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

func floatPtr(v float64) *float64 { return &v }

func TestProvidersFromEndpointsAggregatesStats(t *testing.T) {
	up5m := 100.0
	up30m := 99.0
	up1d := 98.0
	endpoints := []components.PublicEndpoint{
		{
			Tag:               "deepinfra/fp8",
			ProviderName:      "DeepInfra",
			ContextLength:     128000,
			Pricing:           components.Pricing{Prompt: "0.000001", Completion: "0.000002"},
			LatencyLast30m:    &components.PercentileStats{P50: 400, P75: 600, P90: 900, P99: 1200},
			ThroughputLast30m: &components.PercentileStats{P50: 60, P75: 50, P90: 30, P99: 10},
			UptimeLast5m:      &up5m,
			UptimeLast30m:     &up30m,
			UptimeLast1d:      &up1d,
		},
		{
			Tag:               "deepinfra/fp8/flex",
			ProviderName:      "DeepInfra",
			ContextLength:     64000,
			Pricing:           components.Pricing{Prompt: "0.000002", Completion: "0.000004"},
			LatencyLast30m:    &components.PercentileStats{P50: 600, P75: 800, P90: 1100, P99: 1400},
			ThroughputLast30m: &components.PercentileStats{P50: 40, P75: 30, P90: 20, P99: 5},
			UptimeLast30m:     floatPtr(97),
		},
		{
			Tag:           "together",
			ProviderName:  "Together",
			ContextLength: 32000,
		},
	}

	providers := providersFromEndpoints(endpoints)
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}

	if providers[0].Slug != "deepinfra" || providers[1].Slug != "together" {
		t.Fatalf("providers are not sorted: %v, %v", providers[0].Slug, providers[1].Slug)
	}

	deepinfra := providers[0]
	if deepinfra.Endpoints != 2 {
		t.Fatalf("expected 2 endpoints, got %d", deepinfra.Endpoints)
	}
	if deepinfra.Context != 128000 {
		t.Fatalf("expected the largest context, got %d", deepinfra.Context)
	}
	if deepinfra.Prompt != 0.000001 {
		t.Fatalf("expected the first listed price, got %v", deepinfra.Prompt)
	}
	if deepinfra.Latency == nil || deepinfra.Latency.P50 != 500 {
		t.Fatalf("latency was not averaged: %#v", deepinfra.Latency)
	}
	if deepinfra.Latency.P90 != 1000 {
		t.Fatalf("latency p90 was not averaged: %#v", deepinfra.Latency)
	}
	if deepinfra.Throughput == nil || deepinfra.Throughput.P50 != 50 {
		t.Fatalf("throughput was not averaged: %#v", deepinfra.Throughput)
	}
	if deepinfra.Uptime5m == nil || *deepinfra.Uptime5m != 100 {
		t.Fatalf("unexpected 5m uptime: %v", deepinfra.Uptime5m)
	}
	if deepinfra.Uptime30m == nil || *deepinfra.Uptime30m != 98 {
		t.Fatalf("uptime was not averaged: %v", deepinfra.Uptime30m)
	}
	if deepinfra.Uptime1d == nil || *deepinfra.Uptime1d != 98 {
		t.Fatalf("unexpected 1d uptime: %v", deepinfra.Uptime1d)
	}

	together := providers[1]
	if together.Latency != nil || together.Throughput != nil || together.Uptime30m != nil {
		t.Fatalf("expected no statistics for together: %#v", together)
	}
}

func TestRoutingRowFormatsStats(t *testing.T) {
	provider := routingProvider{
		Slug:       "deepinfra",
		Context:    128000,
		Uptime30m:  floatPtr(98.25),
		Latency:    &components.PercentileStats{P50: 1500},
		Throughput: &components.PercentileStats{P50: 65},
		Endpoints:  2,
	}

	row := routingRow(provider, "1")
	if row[2] != "98.2%" {
		t.Fatalf("unexpected uptime cell: %q", row[2])
	}
	if row[3] != "1.5s" {
		t.Fatalf("unexpected latency cell: %q", row[3])
	}
	if row[4] != "65" {
		t.Fatalf("unexpected throughput cell: %q", row[4])
	}

	empty := routingRow(routingProvider{Slug: "manual"}, "-")
	for _, cell := range []string{empty[2], empty[3], empty[4]} {
		if cell != "-" {
			t.Fatalf("expected missing stats to render as -: %v", empty)
		}
	}
}

func TestRoutingProviderEffectivePricing(t *testing.T) {
	discount := 0.5
	provider := routingProvider{
		Priced:     true,
		Prompt:     0.000002,
		Completion: 0.000004,
		Discount:   &discount,
	}
	if got := provider.effectivePrompt(); got != 0.000001 {
		t.Fatalf("unexpected effective prompt price: %v", got)
	}
	if got := provider.effectiveCompletion(); got != 0.000002 {
		t.Fatalf("unexpected effective completion price: %v", got)
	}
	if got := routingRow(provider, "1")[7]; got != "$2.000" {
		t.Fatalf("expected discounted completion price, got %q", got)
	}

	free := routingProvider{Priced: true}
	if got := routingRow(free, "1")[7]; got != "Free" {
		t.Fatalf("expected a known zero price to render as Free, got %q", got)
	}
	if got := routingRow(routingProvider{}, "1")[7]; got != "-" {
		t.Fatalf("expected unknown pricing to render as -, got %q", got)
	}
}

func TestRoutingHeatmapRanksSpeedAndCost(t *testing.T) {
	m, _ := testModel(t)
	m.routing.providers = []routingProvider{
		{Slug: "fast-cheap", Priced: true, Completion: 0.000001, Throughput: &components.PercentileStats{P50: 120}},
		{Slug: "fast-pricey", Priced: true, Completion: 0.000010, Throughput: &components.PercentileStats{P50: 100}},
		{Slug: "slow-cheap", Priced: true, Completion: 0.000002, Throughput: &components.PercentileStats{P50: 20}},
		{Slug: "slow-pricey", Priced: true, Completion: 0.000020, Throughput: &components.PercentileStats{P50: 10}},
		{Slug: "manual"},
	}

	heat := m.routingHeatmap()
	for _, slug := range []string{"fast-cheap", "fast-pricey", "slow-cheap", "slow-pricey"} {
		if !heat[slug].HasValue {
			t.Fatalf("%s should have a combined value score", slug)
		}
	}
	if got := heat["fast-cheap"].Value; got != 1 {
		t.Fatalf("fast-cheap should rank first: %#v", heat["fast-cheap"])
	}
	if got := heat["slow-pricey"].Value; got != 0 {
		t.Fatalf("slow-pricey should rank last: %#v", heat["slow-pricey"])
	}
	if got := heat["fast-pricey"].Value; got < 0.49 || got > 0.51 {
		t.Fatalf("fast but expensive should score about 0.5: %#v", heat["fast-pricey"])
	}

	missing := m.routingHeatmap()["manual"]
	if missing.HasValue {
		t.Fatalf("providers without data should not get a value score: %#v", missing)
	}
}

func TestRoutingSortModes(t *testing.T) {
	m, _ := testModel(t)
	m.routing.initialized = true
	m.routing.table = newRoutingTable()
	m.routing.providers = []routingProvider{
		{Slug: "charlie", Priced: true, Completion: 0.000002, Throughput: &components.PercentileStats{P50: 50}},
		{Slug: "alpha", Priced: true, Completion: 0.000004, Throughput: &components.PercentileStats{P50: 20}},
		{Slug: "bravo", Priced: true, Completion: 0.000001, Throughput: &components.PercentileStats{P50: 100}},
		{Slug: "manual"},
	}
	m.refreshRoutingRows()
	assertRoutingSlugs(t, m, "charlie", "alpha", "bravo", "manual")

	m.setRoutingSort(routingSortName)
	assertRoutingSlugs(t, m, "alpha", "bravo", "charlie", "manual")
	if got := m.routingSortLabel(); got != "name↑" {
		t.Fatalf("unexpected sort label: %q", got)
	}
	m.setRoutingSort(routingSortName)
	assertRoutingSlugs(t, m, "manual", "charlie", "bravo", "alpha")
	if got := m.routingSortLabel(); got != "name↓" {
		t.Fatalf("unexpected sort label: %q", got)
	}

	m.setRoutingSort(routingSortThroughput)
	assertRoutingSlugs(t, m, "bravo", "charlie", "alpha", "manual")
	if got := m.routingSortLabel(); got != "t/s↓" {
		t.Fatalf("unexpected sort label: %q", got)
	}
	m.setRoutingSort(routingSortThroughput)
	assertRoutingSlugs(t, m, "alpha", "charlie", "bravo", "manual")

	m.setRoutingSort(routingSortCost)
	assertRoutingSlugs(t, m, "bravo", "charlie", "alpha", "manual")
	if got := m.routingSortLabel(); got != "cost↑" {
		t.Fatalf("unexpected sort label: %q", got)
	}
	m.setRoutingSort(routingSortCost)
	assertRoutingSlugs(t, m, "alpha", "charlie", "bravo", "manual")

	m.setRoutingSort(routingSortValue)
	assertRoutingSlugs(t, m, "bravo", "charlie", "alpha", "manual")
	if got := m.routingSortLabel(); got != "value↓" {
		t.Fatalf("unexpected sort label: %q", got)
	}
	m.setRoutingSort(routingSortValue)
	assertRoutingSlugs(t, m, "alpha", "charlie", "bravo", "manual")

	m.clearRoutingSort()
	assertRoutingSlugs(t, m, "charlie", "alpha", "bravo", "manual")
	if got := m.routingSortLabel(); got != "order" {
		t.Fatalf("unexpected sort label after clearing: %q", got)
	}
}

func TestRoutingSortKeys(t *testing.T) {
	m, _ := testModel(t)
	m.routing.initialized = true
	m.routing.table = newRoutingTable()
	m.routing.providers = []routingProvider{
		{Slug: "alpha", Priced: true, Completion: 0.000001, Throughput: &components.PercentileStats{P50: 10}},
		{Slug: "bravo", Priced: true, Completion: 0.000002, Throughput: &components.PercentileStats{P50: 20}},
	}
	m.refreshRoutingRows()

	m.updateRouting(key('n'))
	if m.routing.sortBy != routingSortName || m.routing.sortDesc {
		t.Fatalf("n should sort by name ascending: %#v", m.routing)
	}
	m.updateRouting(key('t'))
	if m.routing.sortBy != routingSortThroughput || !m.routing.sortDesc {
		t.Fatalf("t should sort by t/s descending: %#v", m.routing)
	}
	m.updateRouting(key('c'))
	if m.routing.sortBy != routingSortCost || m.routing.sortDesc {
		t.Fatalf("c should sort by cost ascending: %#v", m.routing)
	}
	m.updateRouting(key('v'))
	if m.routing.sortBy != routingSortValue || !m.routing.sortDesc {
		t.Fatalf("v should sort by value descending: %#v", m.routing)
	}
	m.updateRouting(key('u'))
	if m.routing.sortBy != routingSortOrder {
		t.Fatalf("u should return to routing order: %#v", m.routing)
	}
}

func TestRoutingSortRequiresMetricData(t *testing.T) {
	m, _ := testModel(t)
	m.routing.initialized = true
	m.routing.table = newRoutingTable()
	m.routing.providers = []routingProvider{
		{Slug: "alpha", Priced: true, Completion: 0.000001},
		{Slug: "bravo", Priced: true, Completion: 0.000002},
	}
	m.refreshRoutingRows()

	m.setRoutingSort(routingSortThroughput)
	if m.routing.sortBy != routingSortOrder || !m.statusIsError {
		t.Fatalf("t/s sorting without throughput data should be refused: %#v", m.routing)
	}

	m.setRoutingSort(routingSortValue)
	if m.routing.sortBy != routingSortOrder || !m.statusIsError {
		t.Fatalf("value sorting without throughput data should be refused: %#v", m.routing)
	}

	// Cost is available, so it should still sort.
	m.setRoutingSort(routingSortCost)
	if m.routing.sortBy != routingSortCost || m.statusIsError {
		t.Fatalf("cost sorting should work with pricing data: %#v", m.routing)
	}
}

func assertRoutingSlugs(t *testing.T, m *Model, want ...string) {
	t.Helper()
	rows := m.routing.table.Rows()
	got := make([]string, len(rows))
	for i := range rows {
		if len(rows[i]) < 2 {
			t.Fatalf("row %d is missing a provider slug: %v", i, rows[i])
		}
		got[i] = rows[i][1]
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected provider order: got %v, want %v", got, want)
	}
}

func TestRoutingSelectionDetailShowsPercentiles(t *testing.T) {
	m, _ := testModel(t)
	m.authenticated = true
	m.view = viewRouting
	m.routing.initialized = true
	m.routing.modelID = "~deepseek/deepseek-flash-latest"
	m.routing.table = newRoutingTable()
	m.resizeRouting(40, 120)
	m.routing.providers = []routingProvider{{
		Slug:       "inference-net",
		Name:       "InferenceNet",
		Uptime5m:   floatPtr(100),
		Uptime30m:  floatPtr(99.5),
		Uptime1d:   floatPtr(98.8),
		Latency:    &components.PercentileStats{P50: 1500, P90: 3200},
		Throughput: &components.PercentileStats{P50: 143, P90: 86},
	}}
	m.routing.order = []string{"inference-net"}
	m.refreshRoutingRows()

	detail := m.routingSelectionDetail()
	for _, want := range []string{"InferenceNet", "up 5m/30m/1d 100.0%", "99.5%", "TTFT p50 1.5s p90 3.2s", "t/s p50 143 p90 86"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q does not contain %q", detail, want)
		}
	}

	view := m.View()
	for _, want := range []string{"Up 30m", "TTFT", "t/s", "Value"} {
		if !strings.Contains(view, want) {
			t.Fatalf("routing view is missing %q:\n%s", want, view)
		}
	}
}

func TestRoutingSelectionDetailWarnsWhenUnauthenticated(t *testing.T) {
	m, _ := testModel(t)
	m.authenticated = false
	m.view = viewRouting
	m.routing.initialized = true
	m.routing.modelID = "~deepseek/deepseek-flash-latest"
	m.routing.table = newRoutingTable()
	m.resizeRouting(40, 120)
	m.routing.providers = []routingProvider{{Slug: "together", Name: "Together", Uptime30m: floatPtr(100)}}
	m.routing.order = []string{"together"}
	m.refreshRoutingRows()

	view := m.View()
	if !strings.Contains(view, "OPENROUTER_API_KEY") {
		t.Fatalf("routing view should hint that a key is required:\n%s", view)
	}
}
