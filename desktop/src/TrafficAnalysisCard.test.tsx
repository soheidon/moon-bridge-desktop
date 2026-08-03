import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { TrafficAnalysisCard } from "./components/TrafficAnalysisCard";

function trafficState(observationCount: number, active = true, relayActive = active, reconciliationStatus?: string, pending: Record<string, boolean> = {}) {
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
        droppedObservations: 0,
      },
    },
    observations: Array.from({ length: observationCount }, (_, index) => ({
      sessionId: "session",
      sequence: index + 1,
      timestamp: "2026-08-03T00:00:00Z",
      direction: "client_to_upstream",
      transport: "http",
      rawPayloadSize: 1,
      decodedObservationSize: 1,
    })),
    pending,
    error: null,
    progress: null,
    lastExport: null,
    exitPromptOpen: false,
    start: () => Promise.resolve(),
    restartCapture: () => Promise.resolve(),
    stop: () => Promise.resolve(),
    finishRelay: () => Promise.resolve(),
    clear: () => Promise.resolve(),
    exportObservations: () => Promise.resolve(),
    revealExport: () => Promise.resolve(),
    restore: () => Promise.resolve(),
    cancelExit: () => Promise.resolve(),
    confirmExit: () => Promise.resolve(),
  };
}

describe("TrafficAnalysisCard layout", () => {
  it("keeps the action footer outside a large observation list", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(2000) as never} />);
    const listIndex = markup.indexOf('class="traffic-observation-list"');
    const footerIndex = markup.indexOf('class="traffic-analysis-footer"');

    expect(listIndex).toBeGreaterThanOrEqual(0);
    expect(footerIndex).toBeGreaterThan(listIndex);
    expect(markup.match(/class="traffic-analysis-footer"/g)).toHaveLength(1);
    expect(markup).toContain('<button class="btn btn-primary" type="button">分析を停止</button>');
  });

  it("keeps save and clear visible but disabled with no observations", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, false) as never} />);

    expect(markup).toMatch(/<button[^>]*disabled=""[^>]*>ログを保存<\/button>/);
    expect(markup).toMatch(/<button[^>]*disabled=""[^>]*>クリア<\/button>/);
    expect(markup).toContain(">分析を開始</button>");
    expect(markup).toContain("観測データがまだないため、ログの保存とクリアはできません。");
  });

  it("shows relay finalization instead of starting analysis after config restore", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1, false, true) as never} />);

    expect(markup).toContain(">中継中</span>");
    expect(markup).toContain("復元済み・再起動待ち");
    expect(markup).toContain(">中継を終了</button>");
    expect(markup).not.toContain(">分析を開始</button>");
  });

  it("keeps finish relay disabled while stop is still running", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(1, false, true, undefined, { stopping: true }) as never} />);

    expect(markup).toMatch(/<button[^>]*disabled=""[^>]*>中継を終了<\/button>/);
  });

  it("shows recovery action after startup reconciliation without treating it as active capture", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, true, false, "pending_restore") as never} />);

    expect(markup).toContain(">復旧要</span>");
    expect(markup).toContain("前回の分析が正常終了していません");
    expect(markup).toContain(">Codex設定を復元</button>");
    expect(markup).not.toContain(">分析を停止</button>");
  });

  it("requires explicit confirmation for a config conflict", () => {
    const markup = renderToStaticMarkup(<TrafficAnalysisCard traffic={trafficState(0, true, false, "config_conflict") as never} />);

    expect(markup).toContain("Codex設定に競合があります");
    expect(markup).toContain(">競合を確認して復元</button>");
  });
});
