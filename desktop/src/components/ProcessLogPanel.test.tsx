import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { formatGatewayLogs, ProcessLogPanel } from "./ProcessLogPanel";
import type { GatewayLog } from "../types/gateway";

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
});


