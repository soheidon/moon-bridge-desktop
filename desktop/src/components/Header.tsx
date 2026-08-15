import type { GatewaySnapshot } from "../types/gateway";

export type AppPage = "dashboard" | "settings" | "traffic";

// Clicking the active sub-page tab returns to the dashboard. Pure so the toggle
// behavior is unit-testable.
export function getNavigationTarget(currentPage: AppPage, clickedPage: AppPage): AppPage {
  if (clickedPage === "dashboard") return "dashboard";
  return currentPage === clickedPage ? "dashboard" : clickedPage;
}

type Props = {
  snapshot: GatewaySnapshot;
  busy: boolean;
  onStart: () => void;
  onStop: () => void;
  gatewayWarning?: string | null;
  page: AppPage;
  onNavigate: (page: AppPage) => void;
};

export function Header({ snapshot, busy, onStart, onStop, gatewayWarning, page, onNavigate }: Props) {
  const running = snapshot.state === "running";
  return (
    <header className="app-header">
      <div className="header-proxy-section">
        {running ? (
          <button className="btn btn-large" disabled={busy} onClick={onStop}>ゲートウェイ停止</button>
        ) : (
          <button className="btn btn-primary btn-large" disabled={busy} onClick={onStart}>ゲートウェイ開始</button>
        )}
        <span className={`status-badge status-${snapshot.state}`}>{label(snapshot.state)}</span>
      </div>
      <div className="header-meta">
        {gatewayWarning && <span className="header-warning">Gateway実行中: {gatewayWarning}</span>}
        <nav className="header-nav" aria-label="ページ切替">
          <span className="version-info">v0.2.0</span>
          <button
            type="button"
            className={`header-nav-tab${page === "traffic" ? " active" : ""}`}
            aria-current={page === "traffic" ? "page" : undefined}
            onClick={() => onNavigate(getNavigationTarget(page, "traffic"))}
          >Codex Traffic Analysis</button>
          <button
            type="button"
            className={`btn btn-settings${page === "settings" ? " active" : ""}`}
            aria-current={page === "settings" ? "page" : undefined}
            onClick={() => onNavigate(getNavigationTarget(page, "settings"))}
          >{page === "settings" ? "設定を閉じる" : "設定"}</button>
        </nav>
      </div>
    </header>
  );
}

function label(state: GatewaySnapshot["state"]) {
  return { stopped: "停止中", starting: "起動中", running: "実行中", stopping: "停止中", error: "エラー" }[state];
}
