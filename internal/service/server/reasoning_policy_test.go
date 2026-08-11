package server

import (
	"testing"

	"moonbridge/internal/format"
)

func TestApplyRoutingReasoningPolicyNormalPreservesRequest(t *testing.T) {
	req := &format.CoreRequest{
		Output:     &format.CoreOutputConfig{Effort: "medium"},
		Thinking:   &format.CoreThinkingConfig{Type: "enabled"},
		Extensions: map[string]any{"openai": map[string]any{"reasoning": map[string]any{"effort": "medium"}, "other": true}},
		MaxTokens:  123,
	}
	if err := applyRoutingReasoningPolicy(req, "normal", nil); err != nil {
		t.Fatalf("apply normal: %v", err)
	}
	if req.Thinking == nil || req.Thinking.Type != "disabled" {
		t.Fatalf("thinking = %#v, want disabled", req.Thinking)
	}
	if req.Output == nil || req.Output.Effort != "" {
		t.Fatalf("output = %#v, want preserved output with empty effort", req.Output)
	}
	openAI := req.Extensions["openai"].(map[string]any)
	if _, ok := openAI["reasoning"]; ok {
		t.Fatal("openai reasoning was not removed")
	}
	if openAI["other"] != true || req.MaxTokens != 123 {
		t.Fatal("unrelated request fields were changed")
	}
}

func TestApplyRoutingReasoningPolicyThinkingCanonicalizesEffort(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"medium", "high"}, {"high", "high"}, {"xhigh", "max"}, {"max", "max"}} {
		t.Run(tc.input, func(t *testing.T) {
			req := &format.CoreRequest{}
			if err := applyRoutingReasoningPolicy(req, "thinking", &tc.input); err != nil {
				t.Fatalf("apply thinking: %v", err)
			}
			if req.Thinking == nil || req.Thinking.Type != "enabled" || req.Output == nil || req.Output.Effort != tc.want {
				t.Fatalf("request = %#v, want enabled/%s", req, tc.want)
			}
		})
	}
}

func TestApplyRoutingReasoningPolicyRejectsInvalidCombinations(t *testing.T) {
	bad := "bogus"
	for _, tc := range []struct {
		name     string
		mode     string
		override *string
	}{
		{"unknown mode", "wat", nil},
		{"thinking nil", "thinking", nil},
		{"normal override", "normal", &bad},
		{"unknown effort", "thinking", &bad},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyRoutingReasoningPolicy(&format.CoreRequest{}, tc.mode, tc.override); err == nil {
				t.Fatal("expected policy error")
			}
		})
	}
}
