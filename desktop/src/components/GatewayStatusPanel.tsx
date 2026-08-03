import type { GatewaySnapshot } from "../types/gateway";

export function GatewayStatusPanel({ snapshot, onOpenConfigFolder }: { snapshot: GatewaySnapshot; onOpenConfigFolder: () => void }) {
  return (
    <section className="panel gateway-panel">
      <div className="panel-header"><h2>Gateway</h2><span className="panel-subtitle">Go sidecar</span></div>
      {snapshot.error && <p className="error-text">{snapshot.error}</p>}
      <dl className="status-grid">
        <div><dt>状態</dt><dd>{snapshot.state}</dd></div>
        <div><dt>待受アドレス</dt><dd><code>{snapshot.address}</code></dd></div>
        <div><dt>設定ファイル</dt><dd className="path-value">{snapshot.configPath}</dd></div>
        <div><dt>プロセスID</dt><dd>{snapshot.pid ?? "—"}</dd></div>
      </dl>
      <button className="btn btn-small" onClick={onOpenConfigFolder}>設定フォルダを開く</button>
    </section>
  );
}
