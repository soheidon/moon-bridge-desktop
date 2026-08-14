import { useEffect, useMemo, useRef, useState } from "react";
import { getWindowSize, setWindowSize } from "../platform/desktop";
import type { GatewayLog } from "../types/gateway";
import { runtimeLogEventMessage, runtimeLogMessages } from "../i18n/runtimeLog";
import type { TrafficRuntimeEvent } from "../types/trafficAnalysis";

export function formatGatewayLogs(logs: GatewayLog[]): string {
  return logs.map((log) => `${log.timestamp} [${log.stream}] ${log.line}`).join("\n");
}

type DisplayLog = {
  timestamp: string;
  line: string;
  stream: GatewayLog["stream"] | "traffic";
  severity?: TrafficRuntimeEvent["severity"];
};

export function mergeRuntimeLogs(logs: GatewayLog[], events: TrafficRuntimeEvent[], locale?: string): DisplayLog[] {
  const combined: Array<DisplayLog & { arrival: number }> = [
    ...logs.map((log, arrival) => ({ ...log, arrival })),
    ...events.map((event, arrival) => ({
      timestamp: event.timestamp,
      line: runtimeLogEventMessage(event.code, locale),
      stream: "traffic" as const,
      severity: event.severity,
      arrival: logs.length + arrival,
    })),
  ];
  return combined
    .sort((left, right) => {
      const leftTime = Date.parse(left.timestamp);
      const rightTime = Date.parse(right.timestamp);
      const safeLeft = Number.isNaN(leftTime) ? Number.POSITIVE_INFINITY : leftTime;
      const safeRight = Number.isNaN(rightTime) ? Number.POSITIVE_INFINITY : rightTime;
      return safeLeft - safeRight || left.arrival - right.arrival;
    })
    .slice(-500)
    .map(({ arrival: _arrival, ...entry }) => entry);
}

export function formatRuntimeLogs(logs: GatewayLog[], events: TrafficRuntimeEvent[], locale?: string): string {
  return mergeRuntimeLogs(logs, events, locale)
    .map((log) => `${log.timestamp} [${log.stream}] ${log.line}`)
    .join("\n");
}

export function ProcessLogPanel({ logs, trafficEvents = [], locale }: { logs: GatewayLog[]; trafficEvents?: TrafficRuntimeEvent[]; locale?: string }) {
  const [collapsed, setCollapsed] = useState(true);
  const [copyState, setCopyState] = useState<"idle" | "copied" | "failed">("idle");
  const logRef = useRef<HTMLPreElement>(null);
  const collapsedWindowSize = useRef<{ w: number; h: number } | null>(null);
  const resizing = useRef(false);
  const text = runtimeLogMessages(locale);
  const displayLogs = useMemo(() => mergeRuntimeLogs(logs, trafficEvents, locale), [logs, trafficEvents, locale]);

  useEffect(() => {
    return () => {
      const size = collapsedWindowSize.current;
      if (!size) return;
      void setWindowSize(size.w, size.h);
      collapsedWindowSize.current = null;
    };
  }, []);

  useEffect(() => {
    if (collapsed || !logRef.current) return;
    logRef.current.scrollTop = logRef.current.scrollHeight;
  }, [collapsed, displayLogs.length]);

  const toggleCollapsed = async () => {
    if (resizing.current) return;
    resizing.current = true;
    const willExpand = collapsed;
    try {
      if (willExpand) {
        const size = await getWindowSize();
        if (size) {
          collapsedWindowSize.current = size;
          await setWindowSize(size.w, size.h + 260);
        }
        setCollapsed(false);
      } else {
        setCollapsed(true);
        const size = collapsedWindowSize.current;
        if (size) {
          await setWindowSize(size.w, size.h);
          collapsedWindowSize.current = null;
        }
      }
    } finally {
      resizing.current = false;
    }
  };

  const copyLogs = async () => {
    if (displayLogs.length === 0) return;
    const copyText = formatRuntimeLogs(logs, trafficEvents, locale);
    try {
      await navigator.clipboard.writeText(copyText);
      setCopyState("copied");
      window.setTimeout(() => setCopyState("idle"), 1800);
    } catch {
      setCopyState("failed");
    }
  };

  return (
    <section className={`panel log-panel ${collapsed ? "collapsed" : "expanded"}`}>
      <div className="panel-header log-panel-header">
        <button className="collapse-header" type="button" onClick={() => void toggleCollapsed()} aria-expanded={!collapsed}>
          <span aria-hidden="true">{collapsed ? "▶" : "▼"}</span>
          <h2>{text.title}</h2>
          <span className="log-line-count">{displayLogs.length} lines</span>
        </button>
        <div className="log-panel-actions">
          {copyState === "copied" && <span className="log-copy-status success-text">{text.copied}</span>}
          {copyState === "failed" && <span className="log-copy-status error-text">{text.copyFailed}</span>}
          <button className="btn btn-small" type="button" disabled={displayLogs.length === 0} onClick={() => void copyLogs()}>{text.copy}</button>
        </div>
      </div>
      {!collapsed && (
        <div className="log-panel-content">
          <pre ref={logRef} className="log-output">{displayLogs.length === 0 ? text.empty : displayLogs.map((log, i) => {
            const className = log.stream === "stderr" ? "log-error" : log.stream === "traffic" ? `log-traffic log-${log.severity}` : "";
            return <span className={className} key={`${log.timestamp}-${i}`}>[{log.stream}] {log.line}{"\n"}</span>;
          })}</pre>
        </div>
      )}
    </section>
  );
}
