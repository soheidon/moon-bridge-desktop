// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import { TrafficAnalysisCard, TrafficExitDialog, exitPromptKind } from "./components/TrafficAnalysisCard";
import type { TrafficCommandError } from "./types/trafficAnalysis";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function trafficState(observationCount: number, active = true, relayActive = active, reconciliationStatus?: string, pending: Record<string, boolean> = {}, error: TrafficCommandError | null = null, overrides: Record<string, unknown> = {}) {
  return {
    status: {
      integrationActive: active,
      relayActive,
      recoveryAvailable: reconciliationStatus !== undefined,
      reconciliationStatus: reconciliationStatus ?? null,
      configPath: "C:/Users/test/.codex/config.toml",
      capture: {
        state: reconciliationStatus ? "stopped" : active ? "capturing" : relayActive ? "passthrough" : "stopped",
        httpRequests: observationCount,
        sseStreams: 0,
        websocketConnections: 0,
        observationCount,
        observationCapacity: 2000,
        droppedObservations: 0,
      },
    },
    observations: Array.from({ length: observationCount }, (_, index) => ({
      sequence: index + 1,
      timestamp: "2026-08-03T00:00:00Z",
      direction: "client_to_upstream",
      transport: "http",
      payloadKind: "json",
      decodingStatus: "identity",
      rawPayloadSize: 1,
      decodedObservationSize: 1,
      disposition: "recorded",
    })),
    pending,
    error,
    progress: null,
    exitPrompt: null,
    start: () => Promise.resolve(),
    restartCapture: () => Promise.resolve(),
    stop: () => Promise.resolve(),
    finishRelay: () => Promise.resolve(),
    finishRelayResolvingConflict: () => Promise.resolve(null),
    clear: () => Promise.resolve(),
    openLogFolder: () => undefined,
    restore: () => Promise.resolve(),
    cancelExit: () => Promise.resolve(),
    confirmExit: () => Promise.resolve(),
    ...overrides,
  };
}

describe("TrafficAnalysisCard layout", () => {
  it("keeps the primary action in the header above a large observation list", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(2000) as never} />);
    const titleIndex = markup.indexOf("Codex Traffic Analysis");
    const stopIndex = markup.indexOf("分析を停止");
    const listIndex = markup.indexOf('class="traffic-observation-list"');

    expect(stopIndex).toBeGreaterThan(titleIndex);
    expect(stopIndex).toBeLessThan(listIndex);
    expect(markup).not.toContain("traffic-analysis-footer");
    expect(markup).toContain('<button class="btn btn-primary" type="button">分析を停止</button>');
  });

  it("keeps the primary action in the title row next to the heading", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1) as never} />);

    const titleRowOpen = markup.indexOf('class="traffic-card-title-row"');
    const titleRowClose = markup.indexOf("</div>", titleRowOpen);
    const titleRow = markup.slice(titleRowOpen, titleRowClose);
    expect(titleRow).toContain("Codex Traffic Analysis");
    expect(titleRow).toContain("分析を停止");
  });

  it("keeps the subtitle right-aligned at the end of the title row, above the status heading", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1) as never} />);

    const titleRowOpen = markup.indexOf('class="traffic-card-title-row"');
    const statusRowOpen = markup.indexOf('class="traffic-header-status-row"');
    const titleRow = markup.slice(titleRowOpen, statusRowOpen);
    const titleIndex = titleRow.indexOf("Codex Traffic Analysis");
    const actionIndex = titleRow.indexOf("分析を停止");
    const subtitleIndex = titleRow.indexOf(">通常のCodex Desktop通信を安全化して観測します</span>");
    expect(titleIndex).toBeGreaterThanOrEqual(0);
    expect(actionIndex).toBeGreaterThan(titleIndex);
    expect(subtitleIndex).toBeGreaterThan(actionIndex);
    expect(markup.indexOf("traffic-status-title")).toBeGreaterThan(statusRowOpen);
    expect(markup.indexOf('class="traffic-card-body"')).toBeGreaterThan(markup.indexOf(">Capture</span>"));
  });

  it("keeps the log folder action enabled and clear disabled with no observations", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, false) as never} />);

    expect(markup).toMatch(/<button[^>]*>ログフォルダーを開く<\/button>/);
    expect(markup).toMatch(/<button[^>]*disabled=""[^>]*>クリア<\/button>/);
    expect(markup).toContain(">分析を開始</button>");
    expect(markup).toContain("観測データがまだないため、クリアはできません。");
    expect(markup).not.toContain("ログを保存");
  });

  it("places the log folder action in the observations header and removes the save/export UI", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1) as never} />);

    const summaryIndex = markup.indexOf("観測サマリー");
    const observationsIndex = markup.indexOf("観測結果");
    const openFolderIndex = markup.indexOf("ログフォルダーを開く");
    expect(summaryIndex).toBeGreaterThanOrEqual(0);
    expect(observationsIndex).toBeGreaterThan(summaryIndex);
    expect(openFolderIndex).toBeGreaterThan(observationsIndex);
    expect(markup).not.toContain("traffic-analysis-footer");
    expect(markup).not.toContain("ログを保存");
    expect(markup).not.toContain("traffic-export-result");
  });

  it("invokes openLogFolder when the observations-header button is clicked", async () => {
    const openLogFolder = vi.fn(() => undefined);
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<TrafficAnalysisCard traffic={trafficState(0, false, false, undefined, {}, null, { openLogFolder }) as never} />);
    });

    const buttons = Array.from(container.querySelectorAll("button"));
    const openFolder = buttons.find((button) => button.textContent?.includes("ログフォルダーを開く"));
    expect(openFolder).toBeDefined();
    expect(openFolder!.closest(".traffic-observations-actions")).not.toBeNull();

    await act(async () => {
      openFolder!.click();
    });

    expect(openLogFolder).toHaveBeenCalledOnce();
    act(() => root.unmount());
  });

  it("orders the header actions as folder, open file, then clear", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1) as never} />);

    const openFolderIndex = markup.indexOf(">ログフォルダーを開く</button>");
    const openFileIndex = markup.indexOf(">ログを開く</button>");
    const clearIndex = markup.indexOf(">クリア</button>");
    expect(openFolderIndex).toBeGreaterThanOrEqual(0);
    expect(openFileIndex).toBeGreaterThan(openFolderIndex);
    expect(clearIndex).toBeGreaterThan(openFileIndex);
  });

  it("invokes openLogFile when the ログを開く button is clicked", async () => {
    const openLogFile = vi.fn(() => undefined);
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<TrafficAnalysisCard traffic={trafficState(0, false, false, undefined, {}, null, { openLogFile }) as never} />);
    });

    const buttons = Array.from(container.querySelectorAll("button"));
    const openFile = buttons.find((button) => button.textContent?.includes("ログを開く"));
    expect(openFile).toBeDefined();
    expect(openFile!.closest(".traffic-observations-actions")).not.toBeNull();

    await act(async () => {
      openFile!.click();
    });

    expect(openLogFile).toHaveBeenCalledOnce();
    act(() => root.unmount());
  });

  it("renders the observation summary as a single row of label: value pairs", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(3) as never} />);

    expect(markup).toContain(">HTTP:</span>");
    expect(markup).toContain("<span class=\"traffic-observation-summary-value\">3</span>");
    expect(markup).not.toContain("traffic-observation-summary-labels");
    expect(markup).not.toContain("traffic-observation-summary-values");
  });

  it("shows relay state as the Capture card and keeps the finish action in the header", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1, false, true) as never} />);

    // 中継中 lives in the Capture status card, not a header chip or footer badge.
    expect(markup).toContain('<span class="traffic-status-value warning">中継中</span>');
    expect(markup).toContain("復元済み・再起動待ち");
    expect(markup).toContain(">中継を終了</button>");
    expect(markup).not.toContain(">分析を開始</button>");
    expect(markup).not.toContain('class="traffic-relay-status"');
  });

  it("keeps finish relay disabled while stop is still running", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1, false, true, undefined, { stopping: true }) as never} />);

    expect(markup).toMatch(/<button[^>]*disabled=""[^>]*>中継を終了<\/button>/);
  });

  it("shows recovery action after startup reconciliation without treating it as active capture", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, true, false, "pending_restore") as never} />);

    // Capture card is stopped (muted), not 実行中; the Codex設定 card flags restore.
    expect(markup).toContain('<span class="traffic-status-value muted">停止中</span>');
    expect(markup).toContain("復元必要");
    expect(markup).toContain("前回の分析が正常終了していません");
    expect(markup).toContain(">Codex設定を復元</button>");
    expect(markup).not.toContain(">分析を停止</button>");
  });

  it("requires explicit confirmation for a config conflict", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, true, false, "config_conflict") as never} />);

    expect(markup).toContain("Codex設定に競合があります");
    expect(markup).toContain(">競合を確認して復元</button>");
  });

  it("shows the recovery card instead of the generic red error after a Start rejection (G1)", () => {
    const recoveryError: TrafficCommandError = {
      operation: "StartTrafficAnalysis",
      operationId: "",
      stage: "traffic",
      code: "traffic_transaction_recovery_required",
      message: "recovery confirmation is required",
      retryable: false,
      configChanged: false,
      captureRunning: false,
      restartCodexRequired: false,
    };
    const markup = renderToStaticMarkup(
      <TrafficAnalysisCard traffic={trafficState(0, true, false, "config_conflict", {}, recoveryError) as never} />,
    );

    expect(markup).toContain("Codex設定に競合があります");
    expect(markup).toContain(">競合を確認して復元</button>");
    // The generic Start-rejection red line (traffic.error.message) must not render
    // while the recovery card is showing. The recovery panel's own config_conflict
    // explanation legitimately reuses the error-text class, so assert on the message.
    expect(markup).not.toContain("recovery confirmation is required（traffic_transaction_recovery_required）");
  });

  it("keeps the generic red error when no recovery card is active (G2)", () => {
    const recoveryError: TrafficCommandError = {
      operation: "StartTrafficAnalysis",
      operationId: "",
      stage: "traffic",
      code: "traffic_transaction_recovery_required",
      message: "recovery confirmation is required",
      retryable: false,
      configChanged: false,
      captureRunning: false,
      restartCodexRequired: false,
    };
    const markup = renderToStaticMarkup(
      <TrafficAnalysisCard traffic={trafficState(0, false, false, undefined, {}, recoveryError) as never} />,
    );

    expect(markup).toContain("class=\"error-text\"");
    expect(markup).toContain("recovery confirmation is required（traffic_transaction_recovery_required）");
  });

  it("shows a recovery_* restore failure inside the recovery card (G1 companion)", () => {
    const restoreError: TrafficCommandError = {
      operation: "RestoreRecovery",
      operationId: "",
      stage: "recovery",
      code: "recovery_required",
      message: "Recovery state cannot be changed safely",
      retryable: false,
      configChanged: false,
      captureRunning: false,
      restartCodexRequired: false,
    };
    const markup = renderToStaticMarkup(
      <TrafficAnalysisCard traffic={trafficState(0, true, false, "config_conflict", {}, restoreError) as never} />,
    );

    expect(markup).toContain("Codex設定に競合があります");
    expect(markup).toContain(">競合を確認して復元</button>");
    // A restore failure (recovery_* code) must be visible so a stuck conflict is
    // diagnosable instead of looking like "nothing happens".
    expect(markup).toContain("Recovery state cannot be changed safely（recovery_required）");
  });
});

describe("TrafficAnalysisCard status cards", () => {
  it("shows the Capture card as 実行中 while capturing", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1) as never} />);

    expect(markup).toContain('<span class="traffic-status-value success">実行中</span>');
  });

  it("shows the Capture card as 中継中 while in passthrough relay", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1, false, true) as never} />);

    expect(markup).toContain('<span class="traffic-status-value warning">中継中</span>');
  });

  it("shows the Capture card as 停止中 when stopped", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, false, false) as never} />);

    expect(markup).toContain('<span class="traffic-status-value muted">停止中</span>');
  });

  it("shows the fixed relay address in the 接続先 card", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, false) as never} />);

    expect(markup).toContain('class="traffic-status-value traffic-status-address"');
    expect(markup).toContain("127.0.0.1:38441");
  });

  it("shows 一時適用中 in the Codex設定 card while capturing", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1) as never} />);

    expect(markup).toContain("一時適用中");
  });

  it("shows 未変更 in the Codex設定 card when nothing changed", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, false, false) as never} />);

    expect(markup).toContain('>未変更</span>');
  });

  it("shows 復元競合 in red in the Codex設定 card for a config conflict", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, true, false, "config_conflict") as never} />);

    expect(markup).toContain('<span class="traffic-status-value error">復元競合</span>');
  });

  it("shows 復元競合 instead of 復元済み・再起動待ち for a live relay conflict (Plan 4t)", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, false, true, "config_conflict") as never} />);

    // The relay is still alive, but an unresolved live conflict must not be
    // masked by the relay-active label: the Codex設定 card flags 復元競合 and the
    // recovery panel is shown with the conflict restore action.
    expect(markup).toContain('<span class="traffic-status-value error">復元競合</span>');
    expect(markup).not.toContain("復元済み・再起動待ち");
    expect(markup).toContain("Codex設定に競合があります");
    expect(markup).toContain(">競合を確認して復元</button>");
  });

  it("omits the 対象 card when there is no target path", () => {
    const emptyTarget = { ...trafficState(0, false, false).status, configPath: "" };
    const markup = renderToStaticMarkup(
      <TrafficAnalysisCard traffic={trafficState(0, false, false, undefined, {}, null, { status: emptyTarget }) as never} />,
    );

    expect(markup).not.toContain(">対象</span>");
  });

  it("shows the 対象 card when a target path exists", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, false) as never} />);

    expect(markup).toContain(">対象</span>");
    expect(markup).toContain("C:/Users/test/.codex/config.toml");
  });

  it("removes the old vertical summary display", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1) as never} />);

    expect(markup).not.toContain("traffic-summary");
  });
});

describe("exitPromptKind", () => {
  it("classifies unsaved observations", () => {
    expect(exitPromptKind({ reason: "unsaved_observations", trafficActive: false, unsavedObservations: true })).toBe("unsaved");
  });

  it("classifies recovery requirements", () => {
    expect(exitPromptKind({ reason: "recovery_required", trafficActive: false, recoveryRequired: true })).toBe("recovery");
  });

  it("classifies unknown reasons with traffic inactive as recovery (safe fallback)", () => {
    expect(exitPromptKind({ reason: "some_future_reason", trafficActive: false })).toBe("recovery");
  });

  it("classifies unknown reasons with traffic active as traffic", () => {
    expect(exitPromptKind({ reason: "some_future_reason", trafficActive: true })).toBe("traffic");
    expect(exitPromptKind({ trafficActive: true })).toBe("traffic");
  });

  it("classifies a gateway-only run", () => {
    expect(exitPromptKind({ reason: "gateway_active", trafficActive: false, gatewayActive: true })).toBe("gateway");
  });

  it("keeps traffic_active precedence even when the gateway is also running", () => {
    expect(exitPromptKind({ reason: "traffic_active", trafficActive: true, gatewayActive: true })).toBe("traffic");
  });
});

describe("TrafficExitDialog", () => {
  it("shows the discard-and-exit action for unsaved observations, without a plain exit button", () => {
    const markup = renderToStaticMarkup(
      <TrafficExitDialog traffic={trafficState(0, false) as never} payload={{ reason: "unsaved_observations", trafficActive: false, unsavedObservations: true }} />,
    );

    expect(markup).toContain("未保存の観測データがあります");
    expect(markup).toContain(">未保存分を破棄して終了</button>");
    expect(markup).not.toContain(">終了する</button>");
    expect(markup).not.toContain("中継を終了してDesktopを閉じる");
  });

  it("shows a plain exit action for recovery requirements", () => {
    const markup = renderToStaticMarkup(
      <TrafficExitDialog traffic={trafficState(0, false) as never} payload={{ reason: "recovery_required", trafficActive: false, recoveryRequired: true }} />,
    );

    expect(markup).toContain("前回の分析が正常終了していません");
    expect(markup).toContain(">終了する</button>");
    expect(markup).not.toContain("未保存分を破棄して終了");
  });

  it("keeps the original relay-running actions for traffic_active", () => {
    const markup = renderToStaticMarkup(
      <TrafficExitDialog traffic={trafficState(1, false, true) as never} payload={{ reason: "traffic_active", trafficActive: true }} />,
    );

    expect(markup).toContain("Codex Traffic Analysisの中継が実行中です");
    expect(markup).toContain(">中継を終了してDesktopを閉じる</button>");
    expect(markup).toContain(">キャンセル</button>");
  });

  it("shows the gateway-stop action for gateway_active", () => {
    const markup = renderToStaticMarkup(
      <TrafficExitDialog traffic={trafficState(0, false) as never} payload={{ reason: "gateway_active", trafficActive: false, gatewayActive: true }} />,
    );

    expect(markup).toContain("Gatewayが実行中です");
    expect(markup).toContain("終了するとMoon Bridge Gatewayを停止します。現在接続中のCodexやクライアントの通信は切断されます。");
    expect(markup).toContain(">Gatewayを停止して終了</button>");
    expect(markup).toContain(">キャンセル</button>");
    expect(markup).not.toContain("中継を終了してDesktopを閉じる");
    expect(markup).not.toContain("未保存分を破棄して終了");
    expect(markup).not.toContain(">終了する</button>");
  });
});
