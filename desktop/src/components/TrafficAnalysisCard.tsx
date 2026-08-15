import type { ExitConfirmationPayload, TrafficObservation, TrafficRequestSummary } from "../types/trafficAnalysis";
import { summarizeTrafficRequests } from "../types/trafficAnalysis";
import type { useTrafficAnalysis } from "../hooks/useTrafficAnalysis";
import { trafficActionDisabled } from "../trafficAnalysisActions";

type TrafficState = ReturnType<typeof useTrafficAnalysis>;

export function TrafficAnalysisCard({ traffic }: { traffic: TrafficState }) {
  const capture = traffic.status?.capture;
  const active = traffic.status?.integrationActive === true && ["starting", "capturing", "draining"].includes(capture?.state ?? "");
  const relayActive = traffic.status?.relayActive === true;
  const recovery = traffic.status?.recoveryAvailable === true && !active;
  const actions = trafficActionDisabled(traffic.observations.length, traffic.pending);
  const codexConfig = codexConfigStatus(active, relayActive, traffic.status);
  const target = traffic.status?.configPath;
  const requestSummaries = summarizeTrafficRequests(traffic.observations);

  return (
    <section className="panel traffic-card" aria-labelledby="traffic-analysis-title">
      <div className="panel-header traffic-card-header">
        <div className="traffic-card-title-row">
          <h2 id="traffic-analysis-title">Codex Traffic Analysis</h2>
          <div className="traffic-card-actions">
            {recovery ? (
              <>
                <button className="btn btn-secondary" type="button" disabled={actions.restart} onClick={() => {
                  if (!window.confirm("この復旧事象でCapture Proxyを一度だけ再起動します。Codex設定は変更しません。続行しますか？")) return;
                  void traffic.restartCapture();
                }}>Captureを再開</button>
                <button className="btn btn-primary" type="button" disabled={actions.restore} onClick={() => {
                  const conflict = traffic.status?.reconciliationStatus === "config_conflict";
                  if (conflict && !window.confirm("Codex設定は外部変更されています。確認の上、分析開始前の設定へ復元して終了しますか？")) return;
                  void traffic.restore(conflict);
                }}>{traffic.status?.reconciliationStatus === "config_conflict" ? "競合を確認して復元" : "Codex設定を復元"}</button>
              </>
            ) : active ? (
              <button className="btn btn-primary" type="button" disabled={actions.stop} onClick={() => void traffic.stop()}>分析を停止</button>
            ) : relayActive ? (
              <button className="btn btn-primary" type="button" disabled={actions.finalize} onClick={() => {
                if (traffic.pending.stopping === true) return;
                void traffic.finishRelayResolvingConflict();
              }}>中継を終了</button>
            ) : (
              <button className="btn btn-primary" type="button" disabled={actions.start} onClick={() => void traffic.start()}>分析を開始</button>
            )}
          </div>
          <span className="panel-subtitle">通常のCodex Desktop通信を安全化して観測します</span>
        </div>
        <div className="traffic-header-status-row">
          <div className="traffic-status">
            <h3 className="traffic-status-title">ステータス</h3>
            <div className="traffic-status-cards">
              <div className="traffic-status-card">
                <span className="traffic-status-label">Capture</span>
                <span className={`traffic-status-value ${captureTone(capture?.state)}`}>{captureLabel(capture?.state)}</span>
              </div>
              <div className="traffic-status-card">
                <span className="traffic-status-label">接続先</span>
                <code className="traffic-status-value traffic-status-address">127.0.0.1:38441</code>
              </div>
              <div className="traffic-status-card">
                <span className="traffic-status-label">Codex設定</span>
                <span className={`traffic-status-value ${codexConfig.tone}`}>{codexConfig.label}</span>
              </div>
              {target && (
                <div className="traffic-status-card">
                  <span className="traffic-status-label">対象</span>
                  <span className="traffic-status-value traffic-status-path">{target}</span>
                </div>
              )}
            </div>
          </div>
        </div>
      </div>

      <div className="traffic-card-body">
        {recovery ? (
          <div className="traffic-card-status traffic-recovery">
            <strong>{recoveryTitle(traffic.status?.reconciliationStatus)}</strong>
            <p>{recoveryMessage(traffic.status?.reconciliationStatus)}</p>
            {traffic.status?.reconciliationStatus === "config_conflict" && (
              <p className="error-text">Codex設定が外部変更されています。確認後にのみ復元できます。</p>
            )}
            {traffic.status?.reconciliationStatus === "config_unreadable" && (
              <p className="error-text">Codex設定を読み込めないため、自動復元は行っていません。</p>
            )}
            {/* A restore failure (recovery_* code) must be visible here; a Start
                rejection (traffic_transaction_*) is already explained by this panel
                and must not be repeated as a generic red line. */}
            {traffic.error?.code.startsWith("recovery_") && <p className="error-text">{traffic.error.message}（{traffic.error.code}）</p>}
            {traffic.progress && <p className="traffic-progress">{traffic.progress.message}</p>}
          </div>
        ) : (
          <>
            <div className="traffic-card-status">
              {active && <p className="traffic-instruction">Codex Desktopを完全終了して再起動後、機密情報を含まないテストを実行してください。</p>}
              {relayActive && !active && <p className="traffic-instruction traffic-relay-instruction">分析の記録を停止しました。Codex Desktopを完全終了して再起動してください。再起動が完了するまでCapture Proxyは通信を中継します。</p>}
              <div className="traffic-observation-summary">
                <h3 className="traffic-observation-summary-title">観測サマリー</h3>
                <div className="traffic-observation-summary-card">
                  <span className="traffic-observation-summary-item"><span className="traffic-observation-summary-label">HTTP:</span><span className="traffic-observation-summary-value">{capture?.httpRequests ?? 0}</span></span>
                  <span className="traffic-observation-summary-item"><span className="traffic-observation-summary-label">SSE:</span><span className="traffic-observation-summary-value">{capture?.sseStreams ?? 0}</span></span>
                  <span className="traffic-observation-summary-item"><span className="traffic-observation-summary-label">WebSocket:</span><span className="traffic-observation-summary-value">{capture?.websocketConnections ?? 0}</span></span>
                  <span className="traffic-observation-summary-item"><span className="traffic-observation-summary-label">観測:</span><span className="traffic-observation-summary-value">{capture?.observationCount ?? 0} / {capture?.observationCapacity ?? 0}</span></span>
                  <span className="traffic-observation-summary-item"><span className="traffic-observation-summary-label">破棄:</span><span className="traffic-observation-summary-value">{capture?.droppedObservations ?? 0}</span></span>
                </div>
              </div>
              {traffic.error && <p className="error-text">{traffic.error.message}（{traffic.error.code}）</p>}
              {traffic.progress && <p className="traffic-progress">{traffic.progress.message}</p>}
            </div>
            <RequestSummaryList summaries={requestSummaries} />
            <ObservationList traffic={traffic} observations={traffic.observations} />
            {traffic.observations.length === 0 && <p className="traffic-empty-hint">観測データがまだないため、クリアはできません。観測の開始後は自動保存されます。</p>}
          </>
        )}
      </div>
    </section>
  );
}

function RequestSummaryList({ summaries }: { summaries: TrafficRequestSummary[] }) {
  if (summaries.length === 0) return null;
  return (
    <div className="traffic-request-summaries" aria-label="リクエスト実効経路サマリー">
      <h3 className="traffic-observation-summary-title">リクエスト実効経路</h3>
      <div className="traffic-request-summary-list">
        {summaries.slice().reverse().map((summary) => (
          <article className="traffic-request-summary" key={summary.requestAlias}>
            <div className="traffic-request-summary-header">
              <strong>{summary.requestAlias}</strong>
              <span>{summary.route} · {summary.transportOutcome} · {summary.statusClass}</span>
            </div>
            <div className="traffic-observation-fields">
              <span>model: {summary.requestedModel}</span>
              <span>slot: {summary.resolvedSlot}</span>
              <span>provider: {summary.provider}</span>
              <span>upstream: {summary.upstreamModel}</span>
              <span>response model: {summary.responseModel}</span>
              <span>mode: {summary.mode}</span>
              <span>thinking: {summary.thinking}</span>
              <span>credential: {summary.credentialState}</span>
              <span>resolver: {summary.resolverState} · generation {summary.resolverGeneration}</span>
              <span>attempts: {summary.attemptCount}</span>
            </div>
          </article>
        ))}
      </div>
    </div>
  );
}

function recoveryTitle(status: string | null | undefined) {
  switch (status) {
    case "config_conflict": return "Codex設定に競合があります";
    case "config_unreadable": return "Codex設定を確認できません";
    case "already_restored": return "Codex設定は復元済みです";
    default: return "前回の分析が正常終了していません";
  }
}

function recoveryMessage(status: string | null | undefined) {
  switch (status) {
    case "config_conflict": return "自動復元は行っていません。内容を確認してから復元してください。";
    case "config_unreadable": return "Codex設定を手動で確認し、読み取り可能な状態にしてから再試行してください。";
    default: return "Codexは現在も127.0.0.1:38441を参照している可能性があります。";
  }
}

function ObservationList({ traffic, observations }: { traffic: TrafficState; observations: TrafficObservation[] }) {
  const actions = trafficActionDisabled(observations.length, traffic.pending);
  return (
    <div className="traffic-observations-panel">
      <div className="traffic-observations-header">
        <h3 className="traffic-observations-title">観測結果</h3>
        <div className="traffic-observations-actions">
          <button className="btn btn-secondary" type="button" disabled={traffic.pending.openingFolder === true} onClick={() => void traffic.openLogFolder()}>ログフォルダーを開く</button>
          <button className="btn btn-secondary" type="button" disabled={traffic.pending.openingFile === true} onClick={() => void traffic.openLogFile()}>ログを開く</button>
          <button className="btn btn-secondary" type="button" disabled={actions.clear} onClick={() => void traffic.clear()}>クリア</button>
        </div>
      </div>
      <div className="traffic-observation-list" aria-label="安全化された観測一覧">
      {observations.length === 0 ? <p className="traffic-empty">まだ観測はありません。</p> : observations.slice().reverse().map((item) => (
        <article className="traffic-observation" key={`${item.transport}-${item.sequence}`}>
          <div className="traffic-observation-head"><strong>#{item.sequence}</strong><span>{item.direction} · {item.transport}</span><time>{new Date(item.timestamp).toLocaleTimeString()}</time></div>
          {item.gatewayEvent ? <div className="traffic-observation-fields">
            <span>{item.kind}</span>
            <span>{item.gatewayEvent.requestAlias}</span>
            {item.gatewayEvent.requestedModel && <span>{item.gatewayEvent.requestedModel}</span>}
            {item.gatewayEvent.routingSlot && <span>slot: {item.gatewayEvent.routingSlot}</span>}
            {item.gatewayEvent.activeProfile && <span>profile: {item.gatewayEvent.activeProfile}</span>}
            {item.gatewayEvent.provider && <span>provider: {item.gatewayEvent.provider}</span>}
            {item.gatewayEvent.upstreamModel && <span>upstream: {item.gatewayEvent.upstreamModel}</span>}
            {item.gatewayEvent.responseModel && <span>response model: {item.gatewayEvent.responseModel}</span>}
            {item.gatewayEvent.mode && <span>mode: {item.gatewayEvent.mode}</span>}
            {item.gatewayEvent.configuredEffort && <span>configured: {item.gatewayEvent.configuredEffort}</span>}
            {item.gatewayEvent.protocol && <span>protocol: {item.gatewayEvent.protocol}</span>}
            {item.gatewayEvent.thinking && <span>thinking: {item.gatewayEvent.thinking}</span>}
            {item.gatewayEvent.effectiveEffort && <span>effective: {item.gatewayEvent.effectiveEffort}</span>}
            {item.gatewayEvent.exchangeIndex !== undefined && <span>exchange: {item.gatewayEvent.exchangeIndex}</span>}
            {item.gatewayEvent.statusCode !== undefined && <span>HTTP {item.gatewayEvent.statusCode}</span>}
            {item.gatewayEvent.streaming && <span>streaming</span>}
          </div> : <div className="traffic-observation-fields">
            <span>{item.method ?? "event"}</span>
            {item.requestAlias && <span>{item.requestAlias}</span>}
            {item.statusCode !== undefined && <span>HTTP {item.statusCode}</span>}
            <span>{item.payloadKind} · {item.decodingStatus}</span>
            <span>{item.rawPayloadSize} B / decoded {item.decodedObservationSize} B</span>
            {item.partial && <span>partial</span>}
            {item.truncated && <span>truncated</span>}
            {item.errorClass && <span className="error-text">{item.errorClass}</span>}
          </div>}
        </article>
      ))}
      </div>
    </div>
  );
}

export type ExitPromptKind = "unsaved" | "recovery" | "traffic" | "gateway";

export function exitPromptKind(payload: ExitConfirmationPayload): ExitPromptKind {
  if (payload.reason === "unsaved_observations") return "unsaved";
  if (payload.reason === "gateway_active") return "gateway";
  if (payload.reason === "recovery_required" || payload.trafficActive !== true) return "recovery";
  return "traffic";
}

export function TrafficExitDialog({ traffic, payload }: { traffic: TrafficState; payload: ExitConfirmationPayload }) {
  const kind = exitPromptKind(payload);
  const exitDisabled = traffic.pending.stopping === true || traffic.pending.finalizing === true;
  const title = kind === "unsaved" ? "未保存の観測データがあります" : kind === "recovery" ? "前回の分析が正常終了していません" : kind === "gateway" ? "Gatewayが実行中です" : "Codex Traffic Analysisの中継が実行中です";
  const body = kind === "unsaved"
    ? "未保存の観測データが残っています。破棄して終了するか、キャンセルしてください。"
    : kind === "recovery"
      ? "復旧の確認が必要な状態です。終了すると復元処理が完了しない可能性があります。Codex Desktop自体は意図的に強制終了しません。"
      : kind === "gateway"
        ? "終了するとMoon Bridge Gatewayを停止します。現在接続中のCodexやクライアントの通信は切断されます。"
        : `${traffic.status?.integrationActive ? "終了すると分析を停止し、Codex設定を元に戻した後、Capture Proxyを終了します。" : "終了するとCapture Proxyを終了します。38441番への既存接続は切断される可能性があります。"} Codex Desktop自体は意図的に強制終了しません。`;
  return (
    <div className="modal-backdrop" role="presentation">
      <div className="modal-card" role="dialog" aria-modal="true" aria-labelledby="traffic-exit-title">
        <h2 id="traffic-exit-title">{title}</h2>
        <p>{body}</p>
        {traffic.error && <p className="error-text">{traffic.error.message}（{traffic.error.code}）</p>}
        <div className="modal-actions">
          <button className="btn" type="button" onClick={() => void traffic.cancelExit()}>キャンセル</button>
          {kind === "unsaved" && (
            <button className="btn btn-primary" type="button" disabled={exitDisabled} onClick={() => void traffic.confirmExit(true)}>未保存分を破棄して終了</button>
          )}
          {kind === "recovery" && (
            <button className="btn btn-primary" type="button" disabled={exitDisabled} onClick={() => void traffic.confirmExit()}>終了する</button>
          )}
          {kind === "traffic" && (
            <button className="btn btn-primary" type="button" disabled={exitDisabled} onClick={() => void traffic.confirmExit()}>{traffic.status?.integrationActive ? "分析を停止して終了" : "中継を終了してDesktopを閉じる"}</button>
          )}
          {kind === "gateway" && (
            <button className="btn btn-primary" type="button" disabled={exitDisabled} onClick={() => void traffic.confirmExit()}>Gatewayを停止して終了</button>
          )}
        </div>
      </div>
    </div>
  );
}

function captureLabel(state: string | undefined) {
  return { capturing: "実行中", starting: "起動中", passthrough: "中継中", draining: "停止処理中", failed: "エラー", stopped: "停止中" }[state ?? "stopped"];
}

function captureTone(state: string | undefined) {
  switch (state) {
    case "capturing": return "success";
    case "starting":
    case "passthrough":
    case "draining": return "warning";
    case "failed": return "error";
    default: return "muted";
  }
}

function codexConfigStatus(active: boolean, relayActive: boolean, status: TrafficState["status"]): { label: string; tone: string } {
  if (active) return { label: "一時適用中", tone: "success" };
  // A live/restart conflict must not be masked by the relay-active label.
  if (status?.reconciliationStatus === "config_conflict" || status?.reconciliationStatus === "config_unreadable") {
    return status.reconciliationStatus === "config_conflict"
      ? { label: "復元競合", tone: "error" }
      : { label: "設定確認不能", tone: "error" };
  }
  if (relayActive) return { label: "復元済み・再起動待ち", tone: "success" };
  switch (status?.reconciliationStatus) {
    case "already_restored": return { label: "復元済み", tone: "success" };
    default: return { label: status?.recoveryAvailable ? "復元必要" : "未変更", tone: status?.recoveryAvailable ? "warning" : "muted" };
  }
}
