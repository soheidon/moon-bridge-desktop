package server

import (
	"encoding/json"
	"testing"

	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/stats"
)

func TestRequestHasImage(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty input", "", false},
		{"null input", "null", false},
		{"string input", `"hello"`, false},
		{"array without image", `[{"type":"input_text","text":"hello"}]`, false},
		{"array with input_image", `[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"data:image/png;base64,abc"}]`, true},
		{"array with image", `[{"type":"text","text":"hello"},{"type":"image","image_url":"data:image/png;base64,abc"}]`, true},
		{"array with image_url", `[{"type":"image_url","image_url":"https://example.com/img.png"}]`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requestHasImage(json.RawMessage(tt.input))
			if got != tt.want {
				t.Errorf("requestHasImage(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestHasModalityImage(t *testing.T) {
	tests := []struct {
		name       string
		modalities []string
		want       bool
	}{
		{"nil list", nil, false},
		{"empty list", []string{}, false},
		{"only text", []string{"text"}, false},
		{"with image", []string{"text", "image"}, true},
		{"only image", []string{"image"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasModalityImage(tt.modalities)
			if got != tt.want {
				t.Errorf("hasModalityImage(%v) = %v, want %v", tt.modalities, got, tt.want)
			}
		})
	}
}

func TestFilterCandidatesByInputNoProviderMgr(t *testing.T) {
	srv := &Server{providerMgr: nil}
	candidates := []provider.ProviderCandidate{
		{ProviderKey: "p1", UpstreamModel: "model-a"},
	}
	filtered, _ := srv.filterCandidatesByInput(candidates, json.RawMessage(`[{"type":"input_image","image_url":"data:image/png;base64,abc"}]`))
	if len(filtered) != 1 {
		t.Fatalf("without providerMgr, should return unchanged: got %d", len(filtered))
	}
}

func TestComputeCostWithProviderPricingNilStats(t *testing.T) {
	cost := computeCostWithProviderPricing(nil, nil, "model", "model", "provider", stats.BillingUsage{})
	if cost != 0 {
		t.Fatalf("nil stats should return 0, got %f", cost)
	}
}
