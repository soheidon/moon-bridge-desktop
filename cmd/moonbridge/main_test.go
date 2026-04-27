package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moonbridge/internal/config"
)

func TestPrintCodexConfigTomlDoesNotSetServiceTier(t *testing.T) {
	var output bytes.Buffer
	err := writeCodexConfigToml(&output, "moonbridge", "http://127.0.0.1:38440/v1", "", config.Config{
		Routes: map[string]config.RouteEntry{
			"moonbridge": {
				Provider:      "openai",
				Model:         "gpt-5.4",
				ContextWindow: 200000,
			},
		},
	})
	if err != nil {
		t.Fatalf("writeCodexConfigToml() error = %v", err)
	}
	generated := output.String()

	for _, notWant := range []string{"service_tier", "flex"} {
		if strings.Contains(generated, notWant) {
			t.Fatalf("generated config should not contain %q:\n%s", notWant, generated)
		}
	}
	for _, want := range []string{
		`model = "moonbridge"`,
		`model_provider = "moonbridge"`,
		`model_context_window = 200000`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated config missing %q:\n%s", want, generated)
		}
	}
}

func TestRunReturnsStartupErrorWithConfigDetails(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	err := os.WriteFile(configPath, []byte(`
mode: Transform
provider:
  providers:
    openai:
      base_url: https://openai.example.test
      api_key: openai-key
      protocol: openai
      models:
        gpt-image-1.5: {}
  routes:
    image: "openai/gpt-image-1.5"
`), 0644)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"-config", configPath, "-print-mode"}, &stdout, &stderr)

	if code != exitStartupErr {
		t.Fatalf("run() exit code = %d, want %d", code, exitStartupErr)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	output := stderr.String()
	for _, want := range []string{
		"Moon Bridge 启动失败：配置文件加载失败",
		"配置文件: " + configPath,
		"providers.openai.protocol must be \"anthropic\" or \"openai-response\"",
		"Responses 直通请使用 openai-response",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr missing %q:\n%s", want, output)
		}
	}
}
