package codexconfig

import (
	"errors"
	"strings"
	"testing"
)

func applyMust(t *testing.T, original string, fields []Field) string {
	t.Helper()
	out, changed, err := Apply([]byte(original), fields)
	if err != nil {
		t.Fatalf("Apply failed: %v\ninput:\n%s", err, original)
	}
	if !changed {
		t.Fatalf("Apply reported no change:\ninput:\n%s\nfields: %+v", original, fields)
	}
	return string(out)
}

func applyNoChange(t *testing.T, original string, fields []Field) {
	t.Helper()
	out, changed, err := Apply([]byte(original), fields)
	if err != nil {
		t.Fatalf("Apply failed: %v\ninput:\n%s", err, original)
	}
	if changed {
		t.Fatalf("Apply changed an already-matching config:\ninput:\n%s\nout:\n%s", original, out)
	}
	if string(out) != original {
		t.Fatalf("no-op Apply returned different bytes:\n%s", out)
	}
}

func wantEditUnsupported(t *testing.T, original string, fields []Field) *Error {
	t.Helper()
	_, _, err := Apply([]byte(original), fields)
	if err == nil {
		t.Fatalf("expected edit_unsupported rejection:\n%s", original)
	}
	var ce *Error
	if !errors.As(err, &ce) || ce.Kind != KindEditUnsupported {
		t.Fatalf("expected codex_config_edit_unsupported, got %v", err)
	}
	return ce
}

func TestApplyReplacesPreservingCommentsAndOrder(t *testing.T) {
	in := `# top comment
model = "gpt-5" # choose a model
model_provider = "openai"

[model_providers.moonbridge]
base_url = "http://old" # old endpoint
`
	out := applyMust(t, in, []Field{
		{Key: "model", Value: "gpt-5o"},
		{Key: "base_url", Table: []string{"model_providers", "moonbridge"}, Value: "http://new"},
	})
	want := `# top comment
model = "gpt-5o" # choose a model
model_provider = "openai"

[model_providers.moonbridge]
base_url = "http://new" # old endpoint
`
	if out != want {
		t.Fatalf("unexpected output:\n%s\nwant:\n%s", out, want)
	}
}

func TestApplyInsertsMissingRootKeyBeforeFirstHeader(t *testing.T) {
	in := `[model_providers.moonbridge]
base_url = "http://x"
`
	out := applyMust(t, in, []Field{{Key: "model", Value: "gpt-5"}})
	want := `model = "gpt-5"
[model_providers.moonbridge]
base_url = "http://x"
`
	if out != want {
		t.Fatalf("unexpected output:\n%s\nwant:\n%s", out, want)
	}
}

func TestApplyInsertsTableKeyBeforeNextHeader(t *testing.T) {
	in := `[model_providers.moonbridge]
base_url = "http://x"

[other]
key = "v"
`
	out := applyMust(t, in, []Field{{Key: "new_key", Table: []string{"model_providers", "moonbridge"}, Value: "hello"}})
	want := `[model_providers.moonbridge]
base_url = "http://x"
new_key = "hello"

[other]
key = "v"
`
	if out != want {
		t.Fatalf("unexpected output:\n%s\nwant:\n%s", out, want)
	}
}

func TestApplyNoOpWhenValueMatches(t *testing.T) {
	applyNoChange(t, "model = \"gpt-5\"\n", []Field{{Key: "model", Value: "gpt-5"}})
}

func TestApplyInsertPreservesTrailingNewline(t *testing.T) {
	in := "model = \"old\"\n"
	out := applyMust(t, in, []Field{{Key: "model_provider", Value: "openai"}})
	want := "model = \"old\"\nmodel_provider = \"openai\"\n"
	if out != want {
		t.Fatalf("unexpected output: %q\nwant: %q", out, want)
	}
}

func TestApplyReplacesQuotedKey(t *testing.T) {
	in := "\"model\" = \"gpt-5\"\n"
	out := applyMust(t, in, []Field{{Key: "model", Value: "gpt-5o"}})
	want := "\"model\" = \"gpt-5o\"\n"
	if out != want {
		t.Fatalf("unexpected output: %q\nwant: %q", out, want)
	}
}

func TestApplyHandlesEqualAndHashInsideString(t *testing.T) {
	in := "model = \"a=b#c\" # real comment\n"
	out := applyMust(t, in, []Field{{Key: "model", Value: "new"}})
	want := "model = \"new\" # real comment\n"
	if out != want {
		t.Fatalf("unexpected output: %q\nwant: %q", out, want)
	}
}

func TestApplyReplacesInt64Value(t *testing.T) {
	in := "model_context_window = 100000\n"
	out := applyMust(t, in, []Field{{Key: "model_context_window", Value: int64(200000)}})
	want := "model_context_window = 200000\n"
	if out != want {
		t.Fatalf("unexpected output: %q\nwant: %q", out, want)
	}
}

func TestApplyPreservesUnknownFields(t *testing.T) {
	in := "custom_thing = \"keep\"\nmodel = \"a\"\n"
	out := applyMust(t, in, []Field{{Key: "model", Value: "b"}})
	if !strings.Contains(out, "custom_thing = \"keep\"") {
		t.Fatalf("unknown field was lost:\n%s", out)
	}
	if !strings.Contains(out, "model = \"b\"") {
		t.Fatalf("model not updated:\n%s", out)
	}
}

func TestApplyMultipleFields(t *testing.T) {
	in := "model = \"a\"\n\n[model_providers.moonbridge]\nbase_url = \"http://x\"\n"
	out := applyMust(t, in, []Field{
		{Key: "model", Value: "b"},
		{Key: "base_url", Table: []string{"model_providers", "moonbridge"}, Value: "http://y"},
	})
	if !strings.Contains(out, "model = \"b\"") || !strings.Contains(out, "base_url = \"http://y\"") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestApplyCRLFOutputPreserved(t *testing.T) {
	in := "model = \"a\"\r\nmodel_provider = \"b\"\r\n"
	out := applyMust(t, in, []Field{{Key: "model", Value: "c"}})
	want := "model = \"c\"\r\nmodel_provider = \"b\"\r\n"
	if out != want {
		t.Fatalf("unexpected output: %q\nwant: %q", out, want)
	}
}

func TestApplyEscapesValueForTOML(t *testing.T) {
	in := "model = \"a\"\n"
	out := applyMust(t, in, []Field{{Key: "model", Value: "he said \"hi\"\nnext"}})
	if !strings.Contains(out, `model = "he said \"hi\"\nnext"`) {
		t.Fatalf("value not TOML-escaped:\n%s", out)
	}
}

func TestApplyRejectsDottedTargetKey(t *testing.T) {
	wantEditUnsupported(t, "model_providers.moonbridge.base_url = \"http://x\"\n",
		[]Field{{Key: "base_url", Table: []string{"model_providers", "moonbridge"}, Value: "http://y"}})
}

func TestApplyRejectsInlineTableTarget(t *testing.T) {
	wantEditUnsupported(t, "model_providers = { moonbridge = { base_url = \"http://x\" } }\n",
		[]Field{{Key: "base_url", Table: []string{"model_providers", "moonbridge"}, Value: "http://y"}})
}

func TestApplyRejectsArrayOfTablesTarget(t *testing.T) {
	wantEditUnsupported(t, "[[model_providers.moonbridge]]\nbase_url = \"http://x\"\n",
		[]Field{{Key: "base_url", Table: []string{"model_providers", "moonbridge"}, Value: "http://y"}})
}

func TestApplyRejectsMultilineStringTarget(t *testing.T) {
	wantEditUnsupported(t, "base_url = \"\"\"\nhttp://x\n\"\"\"\n",
		[]Field{{Key: "base_url", Table: []string{"model_providers", "moonbridge"}, Value: "http://y"}})
}

func TestApplyRejectsDuplicateKey(t *testing.T) {
	wantEditUnsupported(t, "model = \"a\"\nmodel = \"b\"\n",
		[]Field{{Key: "model", Value: "c"}})
}

func TestApplyRejectsDuplicateTable(t *testing.T) {
	wantEditUnsupported(t, "[a]\nx = 1\n[a]\ny = 2\n",
		[]Field{{Key: "z", Table: []string{"a"}, Value: 1}})
}

func TestApplyRejectsQuotedBareDuplicate(t *testing.T) {
	wantEditUnsupported(t, "model = \"a\"\n\"model\" = \"b\"\n",
		[]Field{{Key: "model", Value: "c"}})
}

func TestApplyRejectsInvalidUTF8(t *testing.T) {
	wantEditUnsupported(t, string([]byte{0xff, 0xfe, '\n'}), []Field{{Key: "model", Value: "c"}})
}

func TestApplyRejectsMixedLineEndings(t *testing.T) {
	wantEditUnsupported(t, "model = \"a\"\r\nmodel_provider = \"b\"\n",
		[]Field{{Key: "model", Value: "c"}})
}

func TestApplyRejectsBareCR(t *testing.T) {
	wantEditUnsupported(t, "model = \"a\"\rmodel_provider = \"b\"",
		[]Field{{Key: "model", Value: "c"}})
}

func TestApplyRejectsMissingHeader(t *testing.T) {
	wantEditUnsupported(t, "[other]\nk = 1\n",
		[]Field{{Key: "base_url", Table: []string{"model_providers", "moonbridge"}, Value: "http://y"}})
}

func TestApplyRejectsInsertIntoArrayOfTables(t *testing.T) {
	wantEditUnsupported(t, "[[model_providers.moonbridge]]\nbase_url = \"http://x\"\n",
		[]Field{{Key: "new_key", Table: []string{"model_providers", "moonbridge"}, Value: "v"}})
}

func TestApplyRejectsDottedTableConstruction(t *testing.T) {
	wantEditUnsupported(t, "model_providers.moonbridge = { base_url = \"http://x\" }\n",
		[]Field{{Key: "new_key", Table: []string{"model_providers", "moonbridge"}, Value: "v"}})
}

func TestApplyParseFailedOnInvalidTOML(t *testing.T) {
	_, _, err := Apply([]byte("model = \"unterminated\n"), []Field{{Key: "model", Value: "c"}})
	var ce *Error
	if !errors.As(err, &ce) || ce.Kind != KindParseFailed {
		t.Fatalf("expected codex_config_parse_failed, got %v", err)
	}
}

func TestApplyEmptyFieldsIsNoOp(t *testing.T) {
	applyNoChange(t, "model = \"a\"\n", nil)
}
