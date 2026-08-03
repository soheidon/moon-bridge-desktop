import type { TrafficObservation } from "../types/trafficAnalysis";
import type { useTrafficAnalysis } from "../hooks/useTrafficAnalysis";
import { trafficActionDisabled } from "../trafficAnalysisActions";

type TrafficState = ReturnType<typeof useTrafficAnalysis>;

export function TrafficAnalysisCard({ traffic }: { traffic: TrafficState }) {
  const capture = traffic.status?.capture;
  const active = traffic.status?.integrationActive === true && ["starting", "capturing", "draining"].includes(capture?.state ?? "");
  const relayActive = traffic.status?.relayActive === true;
  const autoSave = traffic.status?.autoSave;
  const recovery = traffic.status?.recoveryAvailable === true && !active;
  const actions = trafficActionDisabled(traffic.observations.length, traffic.pending);
  const stateLabel = active ? "実行中" : relayActive ? "中継中" : recovery ? "復旧要" : "停止中";

  return (
    <section className="panel traffic-card" aria-labelledby="traffic-analysis-title">
      <div className="panel-header">
        <div>
          <h2 id="traffic-analysis-title">Codex Traffic Analysis</h2>
          <span className="panel-subtitle">通常のCodex Desktop通信を安全化して観測します</span>
        </div>
        <span className={`deepseek-state ${active || relayActive ? "active" : recovery ? "inactive" : "muted"}`}>{stateLabel}</span>
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
          </div>
        ) : (
          <>
            <div className="traffic-card-status">
              <dl className="traffic-summary">
                <div><dt>Capture</dt><dd>{captureLabel(capture?.state)}</dd></div>
                <div><dt>接続先</dt><dd><code>127.0.0.1:38441</code></dd></div>
                <div><dt>Codex設定</dt><dd>{active ? "openai_base_urlを一時適用中" : relayActive ? "復元済み・再起動待ち" : "未変更"}</dd></div>
                <div><dt>対象</dt><dd className="path-value">{traffic.status?.configPath ?? "確認中"}</dd></div>
              </dl>
              <div className="traffic-autosave-status">
                <strong>自動保存：{autoSave?.enabled ? "有効" : "準備中"}</strong>
                {autoSave?.destination && <code>{autoSave.destination}</code>}
                {autoSave?.lastSyncedAt && <span>最終同期 {new Date(autoSave.lastSyncedAt).toLocaleTimeString()}</span>}
                {autoSave?.enabled && <span>保存済み {autoSave.observationsWritten}件</span>}
                {autoSave?.lastSafeError && <p className="error-text">{autoSave.lastSafeError.message}</p>}
                {autoSave?.lastSafeError && <button className="btn btn-small" type="button" disabled={traffic.pending.retryingAutosave === true} onClick={() => void traffic.retryAutosave()}>保存を再試行</button>}
                <button className="btn btn-small" type="button" disabled={traffic.pending.openingFolder === true} onClick={() => void traffic.openLogFolder()}>ログフォルダーを開く</button>
              </div>
              {active && <p className="traffic-instruction">Codex Desktopを完全終了して再起動後、機密情報を含まないテストを実行してください。</p>}
              {relayActive && !active && <p className="traffic-instruction traffic-relay-instruction">分析の記録を停止しました。Codex Desktopを完全終了して再起動してください。再起動が完了するまでCapture Proxyは通信を中継します。</p>}
              <div className="traffic-metrics">
                <span>HTTP {capture?.httpRequests ?? 0}</span>
                <span>SSE {capture?.sseStreams ?? 0}</span>
                <span>WebSocket {capture?.websocketConnections ?? 0}</span>
                <span>観測 {traffic.observations.length} / 2000</span>
                <span>破棄 {capture?.droppedObservations ?? 0}</span>
              </div>
              {traffic.observations.length === 0 && <p className="traffic-empty-hint">観測データがまだないため、ログの保存とクリアはできません。手動ログが対象です。自動保存は分析開始時から有効です。</p>}
              {traffic.error && <p className="error-text">{traffic.error.message}（{traffic.error.code}）</p>}
              {traffic.progress && <p className="traffic-progress">{traffic.progress.message}</p>}
              {traffic.lastExport && <div className="traffic-export-result"><strong>保存しました（{traffic.lastExport.observationCount}件）</strong><code>{traffic.lastExport.destination}</code><button className="btn btn-small" type="button" disabled={traffic.pending.revealing === true} onClick={() => void traffic.revealExport()}>保存先フォルダーを開く</button></div>}
            </div>
            <ObservationList observations={traffic.observations} />
          </>
        )}

        <footer className="traffic-analysis-footer">
          {recovery ? (
            <>
              <button className="btn btn-primary" type="button" disabled={actions.restart} onClick={() => {
                if (!window.confirm("この復旧事象でCapture Proxyを一度だけ再起動します。Codex設定は変更しません。続行しますか？")) return;
                void traffic.restartCapture();
              }}>Captureを再開</button>
              <button className="btn btn-secondary" type="button" disabled={actions.restore} onClick={() => {
                const conflict = traffic.status?.reconciliationStatus === "config_conflict";
                if (conflict && !window.confirm("Codex設定は外部変更されています。現在のopenai_base_urlを保持したまま、Capture用設定を復元しますか？")) return;
                void traffic.restore(conflict);
              }}>{traffic.status?.reconciliationStatus === "config_conflict" ? "競合を確認して復元" : "Codex設定を復元"}</button>
            </>
          ) : (
            <>
              <button className="btn btn-secondary" type="button" disabled={actions.export} onClick={() => void traffic.exportObservations()}>ログを保存</button>
              <button className="btn btn-secondary" type="button" disabled={actions.clear} onClick={() => void traffic.clear()}>クリア</button>
              {active ? (
                <button className="btn btn-primary" type="button" disabled={actions.stop} onClick={() => void traffic.stop()}>分析を停止</button>
              ) : relayActive ? (
                <button className="btn btn-primary" type="button" disabled={actions.finalize} onClick={() => {
                  if (traffic.pending.stopping === true) return;
                  if (autoSave?.lastSafeError && !window.confirm("未保存の観測を破棄して中継を終了しますか？")) return;
                  void traffic.finishRelay(Boolean(autoSave?.lastSafeError));
                }}>中継を終了</button>
              ) : (
                <button className="btn btn-primary" type="button" disabled={actions.start} onClick={() => void traffic.start()}>分析を開始</button>
              )}
            </>
          )}
        </footer>
      </div>

      {traffic.exitPromptOpen && <TrafficExitDialog traffic={traffic} />}
    </section>
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

function ObservationList({ observations }: { observations: TrafficObservation[] }) {
  return (
    <div className="traffic-observation-list" aria-label="安全化された観測一覧">
      {observations.length === 0 ? <p className="traffic-empty">まだ観測はありません。</p> : observations.slice().reverse().map((item) => (
        <article className="traffic-observation" key={`${item.sessionId}-${item.sequence}`}>
          <div className="traffic-observation-head"><strong>#{item.sequence}</strong><span>{item.direction} · {item.transport}</span><time>{new Date(item.timestamp).toLocaleTimeString()}</time></div>
          <div className="traffic-observation-fields">
            <span>{item.method ?? item.websocketMessageType ?? "event"}</span>
            <code>{item.receivedPath ?? item.upstreamPath ?? "—"}</code>
            {item.statusCode !== undefined && <span>HTTP {item.statusCode}</span>}
            {item.contentType && <span>{item.contentType}</span>}
            {item.payloadShape?.modelValue && <span>model: {item.payloadShape.modelValue}</span>}
            {item.sseEventType && <span>event: {item.sseEventType}</span>}
            <span>{item.rawPayloadSize} B / decoded {item.decodedObservationSize} B</span>
            <span>opaque {item.opaqueFields?.length ?? 0}</span>
            {item.truncated && <span>truncated</span>}
            {item.errorClass && <span className="error-text">{item.errorClass}</span>}
          </div>
          {item.payloadShape?.topLevelFields && item.payloadShape.topLevelFields.length > 0 && <small>fields: {item.payloadShape.topLevelFields.join(", ")}</small>}
        </article>
      ))}
    </div>
  );
}

function TrafficExitDialog({ traffic }: { traffic: TrafficState }) {
  return (
    <div className="modal-backdrop" role="presentation">
      <div className="modal-card" role="dialog" aria-modal="true" aria-labelledby="traffic-exit-title">
        <h2 id="traffic-exit-title">Codex Traffic Analysisの中継が実行中です</h2>
        <p>{traffic.status?.integrationActive ? "終了すると分析を停止し、Codex設定を元に戻した後、Capture Proxyを終了します。" : "終了するとCapture Proxyを終了します。38441番への既存接続は切断される可能性があります。"} Codex Desktop自体は意図的に強制終了しません。</p>
        {traffic.error && <p className="error-text">{traffic.error.message}（{traffic.error.code}）</p>}
        <div className="modal-actions">
          <button className="btn" type="button" onClick={() => void traffic.cancelExit()}>キャンセル</button>
          {traffic.status?.autoSave.lastSafeError && <button className="btn btn-secondary" type="button" disabled={traffic.pending.stopping === true || traffic.pending.finalizing === true} onClick={() => void traffic.confirmExit(true)}>未保存分を破棄して終了</button>}
          <button className="btn btn-primary" type="button" disabled={traffic.pending.stopping === true || traffic.pending.finalizing === true} onClick={() => void traffic.confirmExit()}>{traffic.status?.integrationActive ? "分析を停止して終了" : "中継を終了してDesktopを閉じる"}</button>
        </div>
      </div>
    </div>
  );
}

function captureLabel(state: string | undefined) {
  return { capturing: "実行中", starting: "起動中", passthrough: "中継中", draining: "停止処理中", failed: "エラー", stopped: "停止中" }[state ?? "stopped"];
}
