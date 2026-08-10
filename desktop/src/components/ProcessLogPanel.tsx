import { useEffect, useRef, useState } from "react";
import { getWindowSize, setWindowSize } from "../platform/desktop";
import type { GatewayLog } from "../types/gateway";

export function formatGatewayLogs(logs: GatewayLog[]): string {
  return logs.map((log) => `${log.timestamp} [${log.stream}] ${log.line}`).join("\n");
}

export function ProcessLogPanel({ logs }: { logs: GatewayLog[] }) {
  const [collapsed, setCollapsed] = useState(true);
  const [copyState, setCopyState] = useState<"idle" | "copied" | "failed">("idle");
  const logRef = useRef<HTMLPreElement>(null);
  const collapsedWindowSize = useRef<{ w: number; h: number } | null>(null);
  const resizing = useRef(false);

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
  }, [collapsed, logs.length]);

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
    if (logs.length === 0) return;
    const text = formatGatewayLogs(logs);
    try {
      await navigator.clipboard.writeText(text);
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
          <h2>起動ログ</h2>
          <span className="log-line-count">{logs.length} lines</span>
        </button>
        <div className="log-panel-actions">
          {copyState === "copied" && <span className="log-copy-status success-text">コピーしました</span>}
          {copyState === "failed" && <span className="log-copy-status error-text">コピーできませんでした</span>}
          <button className="btn btn-small" type="button" disabled={logs.length === 0} onClick={() => void copyLogs()}>コピー</button>
        </div>
      </div>
      {!collapsed && (
        <div className="log-panel-content">
          <pre ref={logRef} className="log-output">{logs.length === 0 ? "まだログはありません。" : logs.map((log, i) => <span className={log.stream === "stderr" ? "log-error" : ""} key={`${log.timestamp}-${i}`}>[{log.stream}] {log.line}{"\n"}</span>)}</pre>
        </div>
      )}
    </section>
  );
}
