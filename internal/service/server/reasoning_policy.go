package server

import (
	"fmt"
	"strings"

	"moonbridge/internal/format"
)

// applyRoutingReasoningPolicy applies the routing-profile policy before a
// provider adapter is called. An empty mode means this is not a routing
// profile request and the client-derived CoreRequest is left untouched.
func applyRoutingReasoningPolicy(req *format.CoreRequest, mode string, override *string) error {
	if req == nil {
		return fmt.Errorf("core request is nil")
	}
	switch mode {
	case "":
		return nil
	case "normal":
		if override != nil {
			return fmt.Errorf("normal mode must not have reasoning")
		}
		req.Thinking = &format.CoreThinkingConfig{Type: "disabled"}
		if req.Output != nil {
			req.Output.Effort = ""
		}
		removeOpenAIReasoning(req)
		return nil
	case "thinking":
		if override == nil || *override == "" {
			return fmt.Errorf("thinking mode requires reasoning")
		}
		effort, err := canonicalRoutingEffort(*override)
		if err != nil {
			return err
		}
		req.Thinking = &format.CoreThinkingConfig{Type: "enabled"}
		if req.Output == nil {
			req.Output = &format.CoreOutputConfig{}
		}
		req.Output.Effort = effort
		return nil
	default:
		return fmt.Errorf("unsupported reasoning mode: %q", mode)
	}
}

func reasoningPolicyErrorCode(err error) string {
	if err == nil {
		return "unsupported_reasoning_mode"
	}
	if strings.Contains(err.Error(), "effort") {
		return "unsupported_reasoning_effort"
	}
	return "unsupported_reasoning_mode"
}

func canonicalRoutingEffort(value string) (string, error) {
	switch value {
	case "low", "medium", "high":
		return "high", nil
	case "xhigh", "max":
		return "max", nil
	default:
		return "", fmt.Errorf("unsupported reasoning effort: %q", value)
	}
}

func removeOpenAIReasoning(req *format.CoreRequest) {
	if req.Extensions == nil {
		return
	}
	openAI, ok := req.Extensions["openai"].(map[string]any)
	if !ok {
		return
	}
	delete(openAI, "reasoning")
}
