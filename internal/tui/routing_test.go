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
	if !strings.Contains(view, "Up 30m") || !strings.Contains(view, "TTFT") || !strings.Contains(view, "t/s") {
		t.Fatalf("routing view is missing performance columns:\n%s", view)
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
