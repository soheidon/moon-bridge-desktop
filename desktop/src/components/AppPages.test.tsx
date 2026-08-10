import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { AppPages } from "./AppPages";
import type { AppPage } from "./Header";
import type { GatewaySnapshot } from "../types/gateway";

const snapshot: GatewaySnapshot = { state: "stopped", address: "127.0.0.1:38440", configPath: "C:/gateway", pid: null, instanceId: null, error: null };

function gatewayStub() {
  return {
    snapshot,
    logs: [],
    busy: false,
    error: null,
    refresh: () => Promise.resolve(),
    start: () => Promise.resolve(),
    stop: () => Promise.resolve(),
    openConfigFolder: () => undefined,
  } as never;
}

function deepseekStub() {
  return {
    status: null,
    metadata: null,
    model: "deepseek-pro",
    setModel: () => undefined,
    reasoningEffort: "high",
    setReasoningEffort: () => undefined,
    reasoningOptions: ["high", "max"],
    saving: false,
    error: null,
    progress: null,
    operationId: null,
    commandError: null,
    connectionTest: null,
    testingConnection: false,
    hasUnsavedChanges: false,
    activateModel: () => Promise.resolve(true),
    refresh: () => Promise.resolve(),
    configure: () => Promise.resolve(true),
    testConnection: () => Promise.resolve(null),
  } as never;
}

function routingStub() {
  return {
    routing: {
      gatewayRunning: false,
      activeProfileId: "",
      profiles: [
        {
          id: "deepseek",
          displayName: "DeepSeek",
          active: false,
          configured: true,
          slots: [
            { id: "sol", displayName: "Sol", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash", reasoning: "max" },
            { id: "terra", displayName: "Terra", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash", reasoning: "high" },
            { id: "luna", displayName: "Luna", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash" },
          ],
        },
      ],
    },
    profiles: [
      {
        id: "deepseek",
        displayName: "DeepSeek",
        active: false,
        configured: true,
        slots: [
          { id: "sol", displayName: "Sol", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash", reasoning: "max" },
          { id: "terra", displayName: "Terra", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash", reasoning: "high" },
          { id: "luna", displayName: "Luna", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash" },
        ],
      },
    ],
    activeProfileId: null,
    gatewayRunning: false,
    activatingProfileId: null,
    saving: false,
    busy: false,
    error: null,
    commandError: null,
    refresh: () => Promise.resolve(),
    activateProfile: () => Promise.resolve(true),
    saveProfile: () => Promise.resolve(true),
  } as never;
}

function trafficStub() {
  return {
    status: {
      integrationActive: false,
      relayActive: false,
      recoveryAvailable: false,
      reconciliationStatus: null,
      configPath: "C:/Users/test/.codex/config.toml",
      capture: { state: "stopped", httpRequests: 0, sseStreams: 0, websocketConnections: 0, observationCount: 0, observationCapacity: 2000, droppedObservations: 0 },
    },
    observations: [],
    progress: null,
    error: null,
    pending: {},
    exitPrompt: null,
    start: () => Promise.resolve(),
    restartCapture: () => Promise.resolve(),
    stop: () => Promise.resolve(),
    finishRelay: () => Promise.resolve(),
    clear: () => Promise.resolve(),
    openLogFolder: () => undefined,
    restore: () => Promise.resolve(),
    refresh: () => Promise.resolve(),
    cancelExit: () => Promise.resolve(),
    confirmExit: () => Promise.resolve(),
  } as never;
}

function render(page: AppPage) {
  return renderToStaticMarkup(<AppPages page={page} snapshot={snapshot} gateway={gatewayStub()} deepseek={deepseekStub()} routing={routingStub()} traffic={trafficStub()} />);
}

describe("AppPages page rendering", () => {
  it("shows gateway status and logs on the dashboard, without the settings or analysis cards", () => {
    const markup = render("dashboard");
    expect(markup).toContain("使用するLLMプロバイダ");
    expect(markup).toContain("DeepSeek");
    expect(markup).toContain("ステータス");
    expect(markup).toContain("待受アドレス");
    expect(markup).toContain("プロセスID");
    expect(markup).toContain("設定ファイル");
    expect(markup).not.toContain(">Gateway</h2>");
    expect(markup).toContain("0 lines");
    expect(markup).not.toContain("まだログはありません。");
    expect(markup).not.toContain("APIキー");
    expect(markup).not.toContain("分析を開始");
  });

  it("shows one Provider settings block with nested DeepSeek routing on the settings page", () => {
    const markup = render("settings");
    expect(markup).toContain("APIキー");
    expect(markup).toContain("DEEPSEEK_API_KEY");
    expect(markup).toContain("キーを保存");
    expect(markup).toContain('class="routing-editor-embedded"');
    expect(markup).not.toContain("モデル設定");
    expect(markup).toContain("Sol");
    expect(markup).toContain("MINIMAX_API_KEY");
    expect(markup).toContain("MOONSHOT_API_KEY");
    expect(markup).toContain("XIAOMI_API_KEY");
    expect(markup).toContain("OPENROUTER_API_KEY");
    expect(markup).toContain("未設定");
    expect(markup).not.toContain("routing-settings-title");
    expect(markup).not.toContain("Codex のルーティング先を設定");
    expect(markup).not.toContain("接続先プロバイダ");
    expect(markup).not.toContain("分析を開始");
    expect(markup).not.toContain("待受アドレス");
  });

  it("does not expose placeholder Provider rows as enabled disclosure buttons", () => {
    const markup = render("settings");
    expect(markup).not.toContain("MiniMax 選択");
    expect(markup).not.toContain("Kimi 選択");
    expect(markup).not.toContain("MiMo 選択");
    expect(markup).not.toContain("OpenRouter 選択");
  });

  it("shows the traffic analysis card on the traffic page", () => {
    const markup = render("traffic");
    expect(markup).toContain("分析を開始");
    expect(markup).toContain("Codex Traffic Analysis");
    expect(markup).not.toContain("APIキー");
    expect(markup).not.toContain("待受アドレス");
  });
});
