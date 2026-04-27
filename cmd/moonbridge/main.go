package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"moonbridge/internal/app"
	"moonbridge/internal/config"
	"moonbridge/internal/logger"
	"moonbridge/internal/server"
)

const (
	exitOK          = 0
	exitRuntimeErr  = 1
	exitStartupErr  = 2
	defaultProgName = "moonbridge"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet(defaultProgName, flag.ContinueOnError)
	flags.SetOutput(stderr)

	configPath := flags.String("config", "", "Path to config.yml")
	addr := flags.String("addr", "", "Override server listen address")
	mode := flags.String("mode", "", "Override mode: CaptureAnthropic, CaptureResponse, or Transform")
	printAddr := flags.Bool("print-addr", false, "Print configured listen address and exit")
	printMode := flags.Bool("print-mode", false, "Print configured mode and exit")
	printDefaultModel := flags.Bool("print-default-model", false, "Print configured default model alias and exit")
	printCodexModel := flags.Bool("print-codex-model", false, "Print configured Codex model and exit")
	printClaudeModel := flags.Bool("print-claude-model", false, "Print configured Claude Code model and exit")
	printCodexConfig := flags.String("print-codex-config", "", "Print Codex config.toml for the model alias and exit")
	codexBaseURL := flags.String("codex-base-url", "", "Base URL to write in generated Codex config")
	codexHome := flags.String("codex-home", "", "CODEX_HOME directory; when set, writes models_catalog.json there")
	if err := flags.Parse(args); err != nil {
		return exitStartupErr
	}

	var cfg config.Config
	var err error
	resolvedConfigPath := config.DefaultConfigPath
	if *configPath != "" {
		resolvedConfigPath = *configPath
	}
	cfg, err = config.LoadFromFile(resolvedConfigPath)
	if err != nil {
		writeStartupError(stderr, "配置文件加载失败", resolvedConfigPath, err,
			"检查 YAML 语法、字段拼写和缩进。",
			"确认 provider、routes、developer.proxy 等必填配置都已补齐。",
			"如果是 protocol 字段，Responses 直通请使用 openai-response。")
		return exitStartupErr
	}
	if err := logger.Init(logger.Config{Level: logger.Level(cfg.LogLevel), Format: cfg.LogFormat, Output: stderr}); err != nil {
		writeStartupError(stderr, "日志初始化失败", resolvedConfigPath, err,
			"检查 log.level 和 log.format 是否为支持的取值。")
		return exitStartupErr
	}
	logger.Info("配置已加载", "path", resolvedConfigPath, "mode", cfg.Mode, "addr", cfg.Addr)
	if *mode != "" {
		cfg.Mode = config.Mode(*mode)
		if err := cfg.Validate(); err != nil {
			writeStartupError(stderr, "配置校验失败", resolvedConfigPath, fmt.Errorf("-mode %q: %w", *mode, err),
				"检查 -mode 是否为 Transform、CaptureResponse 或 CaptureAnthropic。",
				"对应模式下的 provider / developer.proxy 配置也必须完整。")
			return exitStartupErr
		}
	}
	if *addr != "" {
		cfg.OverrideAddr(*addr)
	}
	if *printAddr {
		fmt.Fprintln(stdout, cfg.Addr)
		return exitOK
	}
	if *printMode {
		fmt.Fprintln(stdout, cfg.Mode)
		return exitOK
	}
	if *printDefaultModel {
		fmt.Fprintln(stdout, cfg.DefaultModelAlias())
		return exitOK
	}
	if *printCodexModel {
		fmt.Fprintln(stdout, cfg.CodexModel())
		return exitOK
	}
	if *printClaudeModel {
		fmt.Fprintln(stdout, cfg.AnthropicProxy.Model)
		return exitOK
	}
	if *printCodexConfig != "" {
		if err := writeCodexConfigToml(stdout, *printCodexConfig, *codexBaseURL, *codexHome, cfg); err != nil {
			writeStartupError(stderr, "生成 Codex 配置失败", resolvedConfigPath, err,
				"确认 -codex-home 目录可写，或去掉 -codex-home 只打印 config.toml。")
			return exitRuntimeErr
		}
		return exitOK
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	if err := app.RunServer(ctx, cfg, stderr); err != nil {
		writeStartupError(stderr, "服务运行失败", resolvedConfigPath, err,
			"检查监听地址是否被占用，以及上游 provider 配置是否可用。")
		return exitRuntimeErr
	}
	return exitOK
}

func valueOrDefault(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// writeModelsCatalog generates a Codex-compatible models_catalog.json from
// provider model catalogs, with routes appended as fallback aliases.
func writeModelsCatalog(path string, cfg config.Config) error {
	catalog := struct {
		Models []server.ModelInfo `json:"models"`
	}{Models: server.BuildModelInfosFromConfig(cfg)}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func writeCodexConfigToml(output io.Writer, modelAlias string, baseURL string, codexHome string, cfg config.Config) error {
	route := cfg.RouteFor(modelAlias)

	// Transform "provider/model" format to "model(provider)" for Codex display.
	if provider, modelName := config.ParseModelRef(modelAlias); provider != "" {
		modelAlias = modelName + "(" + provider + ")"
	}
	fmt.Fprintf(output, "model = %q\n", modelAlias)
	fmt.Fprintln(output, `model_provider = "moonbridge"`)
	if route.ContextWindow > 0 {
		fmt.Fprintf(output, "model_context_window = %d\n", route.ContextWindow)
	}
	if route.MaxOutputTokens > 0 {
		fmt.Fprintf(output, "model_max_output_tokens = %d\n", route.MaxOutputTokens)
	}

	// Write models catalog JSON so Codex uses our metadata instead of bundled presets.
	if codexHome != "" {
		catalogPath := filepath.Join(codexHome, "models_catalog.json")
		if err := writeModelsCatalog(catalogPath, cfg); err != nil {
			return fmt.Errorf("write models catalog: %w", err)
		}
		fmt.Fprintf(output, "model_catalog_json = %q\n", catalogPath)
	}

	fmt.Fprintln(output)
	fmt.Fprintln(output, "[model_providers.moonbridge]")
	fmt.Fprintln(output, `name = "Moon Bridge"`)
	fmt.Fprintf(output, "base_url = %q\n", valueOrDefault(baseURL, "http://"+config.DefaultAddr+"/v1"))
	fmt.Fprintln(output, `env_key = "MOONBRIDGE_CLIENT_API_KEY"`)
	fmt.Fprintln(output, `wire_api = "responses"`)
	fmt.Fprintln(output)
	fmt.Fprintln(output, "[mcp_servers.deepwiki]")
	fmt.Fprintln(output, `url = "https://mcp.deepwiki.com/mcp"`)
	fmt.Fprintln(output, "startup_timeout_sec = 3600")
	fmt.Fprintln(output, "tool_timeout_sec = 3600")
	return nil
}

func writeStartupError(output io.Writer, title string, configPath string, err error, hints ...string) {
	fmt.Fprintf(output, "Moon Bridge 启动失败：%s\n", title)
	if configPath != "" {
		fmt.Fprintf(output, "配置文件: %s\n", configPath)
	}
	fmt.Fprintln(output, "错误详情:")
	for i, msg := range errorChain(err) {
		fmt.Fprintf(output, "  %d. %s\n", i+1, msg)
	}
	if len(hints) == 0 {
		return
	}
	fmt.Fprintln(output, "处理建议:")
	for _, hint := range hints {
		fmt.Fprintf(output, "  - %s\n", hint)
	}
}

func errorChain(err error) []string {
	if err == nil {
		return []string{"<nil>"}
	}
	var messages []string
	for current := err; current != nil; current = errors.Unwrap(current) {
		messages = append(messages, current.Error())
	}
	return messages
}
