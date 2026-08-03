import type { GatewayLog } from "../types/gateway";
import { useState } from "react";

export function ProcessLogPanel({ logs }: { logs: GatewayLog[] }) {
  const [copyState, setCopyState] = useState<"idle" | "copied" | "failed">("idle");
  const copyLogs = async () => {
    if (logs.length === 0) return;
    const text = logs.map((log) => `${log.timestamp} [${log.stream}] ${log.line}`).join("\n");
    try {
      await navigator.clipboard.writeText(text);
      setCopyState("copied");
      window.setTimeout(() => setCopyState("idle"), 1800);
    } catch {
      setCopyState("failed");
    }
  };

  return (
    <section className="panel log-panel">
      <div className="panel-header">
        <h2>起動ログ</h2>
        <div className="log-panel-actions">
          {copyState === "copied" && <span className="log-copy-status success-text">コピーしました</span>}
          {copyState === "failed" && <span className="log-copy-status error-text">コピーできませんでした</span>}
          <button className="btn btn-small" type="button" disabled={logs.length === 0} onClick={() => void copyLogs()}>コピー</button>
          <span className="panel-subtitle">stdout / stderr</span>
        </div>
      </div>
      <pre className="log-output">{logs.length === 0 ? "まだログはありません。" : logs.map((log, i) => <span className={log.stream === "stderr" ? "log-error" : ""} key={`${log.timestamp}-${i}`}>[{log.stream}] {log.line}{"\n"}</span>)}</pre>
    </section>
  );
}
