import type { CommandError } from "../platform/desktop";
import type { GatewaySnapshot } from "../types/gateway";

export function GatewayStatusPanel({ snapshot, error }: { snapshot: GatewaySnapshot; error?: CommandError | null }) {
  return (
    <section className="panel gateway-panel">
      <div className="panel-header"><h2>ステータス</h2></div>
      {error?.message && <p className="error-text">{error.message}</p>}
      {snapshot.error && <p className="error-text">{snapshot.error}</p>}
      <dl className="status-grid">
        <div className="status-card"><dt>状態</dt><dd className={`status-card-value status-${snapshot.state}`}>{snapshot.state}</dd></div>
        <div className="status-card"><dt>待受アドレス</dt><dd className="status-card-value"><code>{snapshot.address}</code></dd></div>
        <div className="status-card status-card-pid"><dt>プロセスID</dt><dd className="status-card-value">{snapshot.pid ?? "—"}</dd></div>
        <div className="status-card status-card-config"><dt>設定ファイル</dt><dd className="status-card-value path-value">{snapshot.configPath}</dd></div>
      </dl>
    </section>
  );
}
