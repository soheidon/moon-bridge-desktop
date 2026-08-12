package codexconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stringPtr(value string) *string { return &value }

func TestReadRootURLRejectsUnsafeExistingURL(t *testing.T) {
	cases := []struct {
		name   string
		config string
	}{
		{name: "userinfo", config: "openai_base_url = \"https://user:password@example.com\"\n"},
		{name: "query", config: "openai_base_url = \"https://example.com/?token=secret\"\n"},
		{name: "fragment", config: "openai_base_url = \"https://example.com/#secret\"\n"},
		{name: "control_character", config: "openai_base_url = \"https://example.com/\\u0001\"\n"},
		{name: "encoded_line_feed", config: "openai_base_url = \"https://example.com/%0a\"\n"},
		{name: "encoded_delete", config: "openai_base_url = \"https://example.com/%7f\"\n"},
		{name: "opaque", config: "openai_base_url = \"http:opaque\"\n"},
		{name: "missing_host", config: "openai_base_url = \"http:///path\"\n"},
		{name: "invalid_scheme", config: "openai_base_url = \"ftp://example.com\"\n"},
		{name: "malformed", config: "openai_base_url = \"http://[::1\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, home, _ := newTestService(t)
			writeFile(t, filepath.Join(home, "config.toml"), tc.config)
			if _, err := svc.ReadRootURL(context.Background()); kindOf(t, err) != KindValidationFailed {
				t.Fatalf("expected validation failure, got %v", err)
			}
		})
	}
}

func TestReadRootURLIgnoresCommentsStringsAndTables(t *testing.T) {
	svc, home, _ := newTestService(t)
	config := "# openai_base_url = \"http://comment.invalid\"\n" +
		"note = 'openai_base_url = \"http://string.invalid\"'\n" +
		"openai_base_url = \"http://root.invalid\"\n" +
		"\n[example]\nopenai_base_url = \"http://table.invalid\"\n"
	writeFile(t, filepath.Join(home, "config.toml"), config)

	state, err := svc.ReadRootURL(context.Background())
	if err != nil {
		t.Fatalf("ReadRootURL failed: %v", err)
	}
	if !state.Present || state.Value != "http://root.invalid" {
		t.Fatalf("unexpected root URL state: %+v", state)
	}
	if state.Hash != hashString(state.Value) || state.ConfigHash != hashBytes([]byte(config)) {
		t.Fatalf("unexpected hashes: %+v", state)
	}
}

func TestReadRoutingIdentityReadsOnlyTopLevelKeys(t *testing.T) {
	svc, home, _ := newTestService(t)
	config := "# model = \"comment-model\"\n" +
		"note = 'model_provider = \"string-provider\"'\n" +
		"model = \"gpt-future\"\n" +
		"model_provider = \"openai\"\n" +
		"\n[profile]\nmodel = \"table-model\"\nmodel_provider = \"table-provider\"\n"
	writeFile(t, filepath.Join(home, "config.toml"), config)

	identity, err := svc.ReadRoutingIdentity(context.Background())
	if err != nil {
		t.Fatalf("ReadRoutingIdentity failed: %v", err)
	}
	if identity.Model != "gpt-future" || identity.ModelProvider != "openai" {
		t.Fatalf("unexpected routing identity: %+v", identity)
	}
	if identity.ConfigHash != hashBytes([]byte(config)) {
		t.Fatalf("unexpected config hash: %q", identity.ConfigHash)
	}
}

func TestPrepareAndCommitRootURLPreservesLosslessConfig(t *testing.T) {
	svc, home, _ := newTestService(t)
	path := filepath.Join(home, "config.toml")
	original := "# keep\nmodel = \"gpt-5\"\n\n[other]\nvalue = \"preserve\" # comment\n"
	writeFile(t, path, original)

	prepared, err := svc.PrepareRootURLChange(context.Background(), stringPtr("http://127.0.0.1:38441"), "")
	if err != nil {
		t.Fatalf("PrepareRootURLChange failed: %v", err)
	}
	if prepared.PreviousPresent || prepared.Present != true || prepared.Value != "http://127.0.0.1:38441" {
		t.Fatalf("unexpected prepared state: %+v", prepared)
	}
	if err := svc.CommitPreparedRootURLChange(context.Background(), prepared); err != nil {
		t.Fatalf("CommitPreparedRootURLChange failed: %v", err)
	}
	got := readFile(t, path)
	want := "# keep\nmodel = \"gpt-5\"\nopenai_base_url = \"http://127.0.0.1:38441\"\n\n[other]\nvalue = \"preserve\" # comment\n"
	if got != want {
		t.Fatalf("lossless root insertion mismatch:\n%s\nwant:\n%s", got, want)
	}
}

func TestPrepareDeleteRootURLPreservesTablesAndCRLF(t *testing.T) {
	svc, home, _ := newTestService(t)
	path := filepath.Join(home, "config.toml")
	original := "openai_base_url = \"http://127.0.0.1:38441\"\r\n# keep\r\n\r\n[other]\r\nvalue = \"keep\"\r\n"
	writeFile(t, path, original)

	prepared, err := svc.PrepareRootURLChange(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("PrepareRootURLChange delete failed: %v", err)
	}
	if !prepared.PreviousPresent || prepared.Present {
		t.Fatalf("unexpected delete state: %+v", prepared)
	}
	if err := svc.CommitPreparedRootURLChange(context.Background(), prepared); err != nil {
		t.Fatalf("CommitPreparedRootURLChange delete failed: %v", err)
	}
	want := "# keep\r\n\r\n[other]\r\nvalue = \"keep\"\r\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("delete changed unrelated TOML:\n%q\nwant:\n%q", got, want)
	}
}

func TestPrepareRootURLRejectsCASConflictWithoutWrite(t *testing.T) {
	svc, home, _ := newTestService(t)
	path := filepath.Join(home, "config.toml")
	original := "model = \"a\"\n"
	writeFile(t, path, original)
	prepared, err := svc.PrepareRootURLChange(context.Background(), stringPtr("http://127.0.0.1:38441"), "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "model = \"externally-changed\"\n")
	if err := svc.CommitPreparedRootURLChange(context.Background(), prepared); kindOf(t, err) != KindConflict {
		t.Fatalf("expected conflict, got %v", err)
	}
	if got := readFile(t, path); strings.Contains(got, "38441") {
		t.Fatalf("CAS conflict wrote the candidate: %s", got)
	}
}

func TestPrepareRootURLRejectsUnsafeURLWithoutWrite(t *testing.T) {
	unsafe := []string{
		"https://user:password@example.com",
		"https://example.com/?token=secret",
		"https://example.com/#secret",
		"not-a-url",
	}
	for _, value := range unsafe {
		t.Run(value, func(t *testing.T) {
			svc, home, _ := newTestService(t)
			path := filepath.Join(home, "config.toml")
			original := "model = \"a\"\n"
			writeFile(t, path, original)
			if _, err := svc.PrepareRootURLChange(context.Background(), stringPtr(value), ""); kindOf(t, err) != KindValidationFailed {
				t.Fatalf("expected validation failure for %q, got %v", value, err)
			}
			if got := readFile(t, path); got != original {
				t.Fatalf("unsafe URL changed config: %q", got)
			}
		})
	}
}

func TestReadRootURLRejectsAmbiguousRootDefinitions(t *testing.T) {
	cases := []string{
		"openai_base_url = \"http://a\"\nopenai_base_url = \"http://b\"\n",
		"openai_base_url = \"\"\"\nhttp://a\n\"\"\"\n",
		"openai_base_url = { value = \"http://a\" }\n",
	}
	for _, config := range cases {
		t.Run(strings.ReplaceAll(config, "\n", "_"), func(t *testing.T) {
			svc, home, _ := newTestService(t)
			writeFile(t, filepath.Join(home, "config.toml"), config)
			_, err := svc.ReadRootURL(context.Background())
			var ce *Error
			if !errors.As(err, &ce) || ce.Kind != KindEditUnsupported {
				t.Fatalf("expected edit unsupported, got %v", err)
			}
		})
	}
}

func TestPrepareRootURLContextCancellationDoesNotReadOrWrite(t *testing.T) {
	svc, home, _ := newTestService(t)
	path := filepath.Join(home, "config.toml")
	writeFile(t, path, "model = \"a\"\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.PrepareRootURLChange(ctx, stringPtr("http://127.0.0.1:38441"), ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
