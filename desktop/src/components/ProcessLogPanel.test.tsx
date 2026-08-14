import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { formatGatewayLogs, formatRuntimeLogs, ProcessLogPanel } from "./ProcessLogPanel";
import type { GatewayLog } from "../types/gateway";
import type { TrafficRuntimeEvent } from "../types/trafficAnalysis";

const logs: GatewayLog[] = [
  { timestamp: "2026-08-08T12:00:00.000Z", stream: "stdout", line: "gateway started" },
  { timestamp: "2026-08-08T12:00:01.000Z", stream: "stderr", line: "warning" },
];

describe("ProcessLogPanel", () => {
  it("starts collapsed and preserves the line count without rendering the body", () => {
    const markup = renderToStaticMarkup(<ProcessLogPanel logs={logs} />);
    expect(markup).toContain("2 lines");
    expect(markup).toContain('aria-expanded="false"');
    expect(markup).not.toContain("gateway started");
  });

  it("keeps stdout/stderr rendering and the original copy payload contract", () => {
    const markup = renderToStaticMarkup(<ProcessLogPanel logs={logs} />);
    expect(formatGatewayLogs(logs)).toBe("2026-08-08T12:00:00.000Z [stdout] gateway started\n2026-08-08T12:00:01.000Z [stderr] warning");
    expect(markup).toContain("log-panel-actions");
  });

  it("keeps the empty state hidden while collapsed and disables copy for empty logs", () => {
    const markup = renderToStaticMarkup(<ProcessLogPanel logs={[]} />);
    expect(markup).toContain("0 lines");
    expect(markup).toContain('disabled=""');
    expect(markup).not.toContain("まだログはありません。");
  });

  it("merges safe Traffic events chronologically and translates the Log title in Japanese and English", () => {
    const trafficEvents: TrafficRuntimeEvent[] = [
      { timestamp: "2026-08-08T12:00:02.000Z", code: "traffic_route_applied", severity: "info" },
      { timestamp: "2026-08-08T12:00:01.000Z", code: "traffic_backup_created", severity: "info" },
      { timestamp: "2026-08-08T12:00:03.000Z", code: "traffic_analysis_started", severity: "success" },
    ];
    const gatewayLogs: GatewayLog[] = [{ timestamp: "2026-08-08T12:00:00.000Z", stream: "stdout", line: "gateway started" }];
    const expected = {
      ja: ["gateway started", "バックアップを作成しました", "Codexの接続先を分析用に切り替えました", "分析を開始しました"],
      en: ["gateway started", "Created a backup", "Switched Codex to the analysis endpoint", "Started analysis"],
    } as const;

    for (const locale of ["ja", "en"] as const) {
      const markup = renderToStaticMarkup(<ProcessLogPanel logs={gatewayLogs} trafficEvents={trafficEvents} locale={locale} />);
      const formatted = formatRuntimeLogs(gatewayLogs, trafficEvents, locale);
      expect(markup).toContain(locale === "ja" ? "ログ" : "Log");
      expect(markup).not.toContain("起動ログ");
      expect(markup).not.toContain("Startup Log");
      expect(formatted).not.toContain("traffic_backup_created");
      for (const line of expected[locale]) expect(formatted).toContain(line);
      expect(formatted.indexOf(expected[locale][0])).toBeLessThan(formatted.indexOf(expected[locale][1]));
    }
  });
});
