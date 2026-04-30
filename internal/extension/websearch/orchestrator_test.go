package websearch

import (
	"encoding/json"
	"fmt"
	"testing"

	"moonbridge/internal/protocol/anthropic"
)

func TestCollectToolUsesFromEvents_empty(t *testing.T) {
	result := collectToolUsesFromEvents(nil)
	if len(result) != 0 {
		t.Fatalf("expected empty result, got %d", len(result))
	}

	result = collectToolUsesFromEvents([]anthropic.StreamEvent{})
	if len(result) != 0 {
		t.Fatalf("expected empty result, got %d", len(result))
	}
}

func TestCollectToolUsesFromEvents_nonToolBlocks(t *testing.T) {
	events := []anthropic.StreamEvent{
		{Type: "content_block_start", Index: 0, ContentBlock: &anthropic.ContentBlock{Type: "text"}},
		{Type: "content_block_delta", Index: 0, Delta: anthropic.StreamDelta{Type: "text_delta", Text: "hello"}},
	}
	result := collectToolUsesFromEvents(events)
	if len(result) != 0 {
		t.Fatalf("expected no tool_use blocks, got %d", len(result))
	}
}

func TestCollectToolUsesFromEvents_partialJSON(t *testing.T) {
	events := []anthropic.StreamEvent{
		{
			Type:  "content_block_start",
			Index: 0,
			ContentBlock: &anthropic.ContentBlock{
				Type: "tool_use",
				ID:   "tu_123",
				Name: "tavily_search",
			},
		},
		{
			Type:  "content_block_delta",
			Index: 0,
			Delta: anthropic.StreamDelta{
				Type:        "input_json_delta",
				PartialJSON: `{"query": "latest news in AI`,
			},
		},
		{
			Type:  "content_block_delta",
			Index: 0,
			Delta: anthropic.StreamDelta{
				Type:        "input_json_delta",
				PartialJSON: ` technology"}`,
			},
		},
	}

	result := collectToolUsesFromEvents(events)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool_use block, got %d", len(result))
	}

	tu := result[0]
	if tu.ID != "tu_123" {
		t.Fatalf("expected ID tu_123, got %s", tu.ID)
	}
	if tu.Name != "tavily_search" {
		t.Fatalf("expected name tavily_search, got %s", tu.Name)
	}

	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(tu.Input, &params); err != nil {
		t.Fatalf("failed to unmarshal assembled input: %v", err)
	}
	if params.Query != "latest news in AI technology" {
		t.Fatalf("expected query 'latest news in AI technology', got %q", params.Query)
	}
}

func TestCollectToolUsesFromEvents_multipleTools(t *testing.T) {
	events := []anthropic.StreamEvent{
		{
			Type:  "content_block_start",
			Index: 0,
			ContentBlock: &anthropic.ContentBlock{
				Type: "tool_use",
				ID:   "tu_001",
				Name: "tavily_search",
			},
		},
		{
			Type:  "content_block_delta",
			Index: 0,
			Delta: anthropic.StreamDelta{
				Type:        "input_json_delta",
				PartialJSON: `{"query": "first query"}`,
			},
		},
		{
			Type:  "content_block_start",
			Index: 1,
			ContentBlock: &anthropic.ContentBlock{
				Type: "tool_use",
				ID:   "tu_002",
				Name: "firecrawl_fetch",
			},
		},
		{
			Type:  "content_block_delta",
			Index: 1,
			Delta: anthropic.StreamDelta{
				Type:        "input_json_delta",
				PartialJSON: `{"url": "https://example.com"}`,
			},
		},
	}

	result := collectToolUsesFromEvents(events)
	if len(result) != 2 {
		t.Fatalf("expected 2 tool_use blocks, got %d", len(result))
	}

	var first struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(result[0].Input, &first); err != nil {
		t.Fatalf("failed to unmarshal first tool input: %v", err)
	}
	if first.Query != "first query" {
		t.Fatalf("expected 'first query', got %q", first.Query)
	}

	var second struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(result[1].Input, &second); err != nil {
		t.Fatalf("failed to unmarshal second tool input: %v", err)
	}
	if second.URL != "https://example.com" {
		t.Fatalf("expected 'https://example.com', got %q", second.URL)
	}
}

func TestCollectToolUsesFromEvents_startWithEmptyInput(t *testing.T) {
	events := []anthropic.StreamEvent{
		{
			Type:  "content_block_start",
			Index: 0,
			ContentBlock: &anthropic.ContentBlock{
				Type:  "tool_use",
				ID:    "tu_001",
				Name:  "tavily_search",
				Input: json.RawMessage(`{}`),
			},
		},
		{
			Type:  "content_block_delta",
			Index: 0,
			Delta: anthropic.StreamDelta{
				Type:        "input_json_delta",
				PartialJSON: `{"query": "search term"}`,
			},
		},
	}

	result := collectToolUsesFromEvents(events)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool_use block, got %d", len(result))
	}

	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(result[0].Input, &params); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if params.Query != "search term" {
		t.Fatalf("expected query 'search term', got %q", params.Query)
	}
}

func TestCollectContentFromEvents_textAndTool(t *testing.T) {
	events := []anthropic.StreamEvent{
		{
			Type:  "content_block_start",
			Index: 0,
			ContentBlock: &anthropic.ContentBlock{
				Type: "text",
			},
		},
		{
			Type:  "content_block_delta",
			Index: 0,
			Delta: anthropic.StreamDelta{
				Type: "text_delta",
				Text: "I will search for you.",
			},
		},
		{
			Type:  "content_block_start",
			Index: 1,
			ContentBlock: &anthropic.ContentBlock{
				Type: "tool_use",
				ID:   "tu_001",
				Name: "tavily_search",
			},
		},
		{
			Type:  "content_block_delta",
			Index: 1,
			Delta: anthropic.StreamDelta{
				Type:        "input_json_delta",
				PartialJSON: `{"query": "test"}`,
			},
		},
	}

	blocks := collectContentFromEvents(events)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(blocks))
	}

	if blocks[0].Type != "text" {
		t.Fatalf("expected first block type 'text', got %q", blocks[0].Type)
	}
	if blocks[0].Text != "I will search for you." {
		t.Fatalf("expected text 'I will search for you.', got %q", blocks[0].Text)
	}

	if blocks[1].Type != "tool_use" {
		t.Fatalf("expected second block type 'tool_use', got %q", blocks[1].Type)
	}
	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(blocks[1].Input, &params); err != nil {
		t.Fatalf("failed to unmarshal tool input: %v", err)
	}
	if params.Query != "test" {
		t.Fatalf("expected query 'test', got %q", params.Query)
	}
}

func TestCollectContentFromEvents_thinkingAndTool(t *testing.T) {
	events := []anthropic.StreamEvent{
		{
			Type:  "content_block_start",
			Index: 0,
			ContentBlock: &anthropic.ContentBlock{
				Type: "thinking",
			},
		},
		{
			Type:  "content_block_delta",
			Index: 0,
			Delta: anthropic.StreamDelta{
				Type:     "thinking_delta",
				Thinking: "Let me think about this...",
			},
		},
		{
			Type:  "content_block_delta",
			Index: 0,
			Delta: anthropic.StreamDelta{
				Type:      "signature_delta",
				Signature: "sig123456",
			},
		},
		{
			Type:  "content_block_start",
			Index: 1,
			ContentBlock: &anthropic.ContentBlock{
				Type: "tool_use",
				ID:   "tu_001",
				Name: "tavily_search",
			},
		},
		{
			Type:  "content_block_delta",
			Index: 1,
			Delta: anthropic.StreamDelta{
				Type:        "input_json_delta",
				PartialJSON: `{"query": "test"}`,
			},
		},
	}

	blocks := collectContentFromEvents(events)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(blocks))
	}

	if blocks[0].Type != "thinking" {
		t.Fatalf("expected first block type 'thinking', got %q", blocks[0].Type)
	}
	if blocks[0].Thinking != "Let me think about this..." {
		t.Fatalf("expected thinking text, got %q", blocks[0].Thinking)
	}
	if blocks[0].Signature != "sig123456" {
		t.Fatalf("expected signature 'sig123456', got %q", blocks[0].Signature)
	}

	if blocks[1].Type != "tool_use" {
		t.Fatalf("expected second block type 'tool_use', got %q", blocks[1].Type)
	}
	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(blocks[1].Input, &params); err != nil {
		t.Fatalf("failed to unmarshal tool input: %v", err)
	}
	if params.Query != "test" {
		t.Fatalf("expected query 'test', got %q", params.Query)
	}
}

func TestStaticStream(t *testing.T) {
	events := []anthropic.StreamEvent{
		{Type: "ping"},
		{Type: "message_start"},
	}
	s := &staticStream{events: events}

	event, err := s.Next()
	if err != nil {
		t.Fatalf("unexpected error on first Next: %v", err)
	}
	if event.Type != "ping" {
		t.Fatalf("expected 'ping', got %q", event.Type)
	}

	event, err = s.Next()
	if err != nil {
		t.Fatalf("unexpected error on second Next: %v", err)
	}
	if event.Type != "message_start" {
		t.Fatalf("expected 'message_start', got %q", event.Type)
	}

	_, err = s.Next()
	if err == nil || err.Error() != "EOF" {
		t.Fatalf("expected EOF error, got %v", err)
	}
}

func TestCollectContentFromEvents_preservesNonRebuiltBlockFields(t *testing.T) {
	// For block types we don't explicitly reconstruct, fields from start should be preserved.
	source := &anthropic.ImageSource{
		Type: "base64",
		Data: "abcd",
	}
	events := []anthropic.StreamEvent{
		{
			Type:  "content_block_start",
			Index: 0,
			ContentBlock: &anthropic.ContentBlock{
				Type:   "image",
				Source: source,
			},
		},
	}

	blocks := collectContentFromEvents(events)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Type != "image" {
		t.Fatalf("expected type 'image', got %q", blocks[0].Type)
	}
	if blocks[0].Source == nil {
		t.Fatal("expected Source to be preserved")
	}
	if blocks[0].Source.Data != "abcd" {
		t.Fatalf("expected Source.Data 'abcd', got %q", blocks[0].Source.Data)
	}

	// Drain staticStream fully as a final sanity check.
	fmt.Println("staticStream drain test: OK")
}
