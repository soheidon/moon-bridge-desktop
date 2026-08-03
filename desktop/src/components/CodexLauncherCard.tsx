import type { useCodexLauncher } from "../hooks/useCodexLauncher";
import type { useDeepSeek } from "../hooks/useDeepSeek";

type CodexState = ReturnType<typeof useCodexLauncher>;
type DeepSeekState = ReturnType<typeof useDeepSeek>;

export function CodexLauncherCard({
  codex,
  deepseek,
  onLaunched,
}: {
  codex: CodexState;
  deepseek: DeepSeekState;
  onLaunched: () => void;
}) {
  async function launch() {
    const succeeded = await codex.launch();
    if (succeeded) onLaunched();
  }

  const effectiveModel = codex.effectiveModel;
  const reasoningOptions = deepseek.status?.allowedReasoningEfforts ?? [];

  return (
    <section className="panel codex-card" aria-labelledby="codex-title">
      <div className="panel-header">
        <div>
          <h2 id="codex-title">Codex</h2>
          <span className="panel-subtitle">Moon Bridge専用環境</span>
        </div>
        <span className={`deepseek-state ${codex.status?.installed ? "active" : "muted"}`}>
          {codex.status?.installed ? "検出済み" : "未確認"}
        </span>
      </div>

      <div className="codex-fields">
        <label>
          <span>プロジェクト</span>
          <div className="codex-project-field">
            <input className="path-value" value={codex.projectDirectory} readOnly placeholder="プロジェクトフォルダを選択してください" />
            <button className="btn btn-secondary" type="button" onClick={() => void codex.chooseProject()} disabled={codex.launching}>
              選択
            </button>
          </div>
        </label>
      </div>

      <dl className="codex-summary">
        <div><dt>モデルルート</dt><dd><code>{codex.routeAlias}</code> → {effectiveModel ? modelLabel(effectiveModel) : "保存済み設定を未取得"}</dd></div>
        <div><dt>Reasoning</dt><dd>{codex.effectiveReasoning ? reasoningLabel(codex.effectiveReasoning) : "保存済み設定を未取得"}{reasoningOptions.length > 0 && <small>（{reasoningOptions.map(reasoningLabel).join(" / ")}）</small>}</dd></div>
        <div><dt>Codex CLI</dt><dd>{codex.status?.installed ? `${codex.status.version ?? "バージョン不明"}` : "未検出"}</dd></div>
        <div><dt>専用CODEX_HOME</dt><dd className="path-value">{codex.status?.codexHome ?? "—"}</dd></div>
      </dl>

      {deepseek.hasUnsavedChanges && <p className="codex-hint">DeepSeekカードに未保存の変更があります。Codexは保存済み設定を使用します。</p>}
      {codex.status && !codex.status.installed && <p className="error-text">Codex CLIが見つかりません。インストール後に再確認してください。</p>}
      {codex.error && <p className="error-text">{codex.error.message}（{codex.error.code}）</p>}
      {codex.progress && <p className="codex-progress">{codex.progress.message}</p>}
      {codex.lastLaunch && <p className="success-text">Codexを起動しました（PID {codex.lastLaunch.terminalPid}）</p>}

      <div className="codex-actions">
        <button className="btn btn-primary" type="button" disabled={codex.launching || !codex.projectDirectory.trim() || !codex.status?.installed} onClick={() => void launch()}>
          {codex.launching ? "起動中…" : "Codexを起動"}
        </button>
        <button className="btn btn-secondary" type="button" disabled={codex.launching} onClick={() => void codex.refreshStatus()}>
          再確認
        </button>
      </div>

      {codex.exitPromptOpen && (
        <div className="modal-backdrop" role="presentation">
          <div className="modal-card" role="dialog" aria-modal="true" aria-labelledby="desktop-exit-title">
            <h2 id="desktop-exit-title">CodexがGatewayを使用している可能性があります</h2>
            <p>Moon Bridge Desktopを終了するとGatewayも停止します。Codexターミナルは残りますが、以後の通信が失敗する可能性があります。</p>
            <div className="modal-actions">
              <button className="btn" type="button" onClick={() => void codex.cancelExit()}>キャンセル</button>
              <button className="btn btn-primary" type="button" onClick={() => void codex.confirmExit()}>終了</button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}

function modelLabel(model: string) {
  return model === "deepseek-v4-pro" ? "DeepSeek V4 Pro" : "DeepSeek V4 Flash";
}

function reasoningLabel(effort: string) {
  return { low: "Low", high: "High", max: "Max" }[effort as "low" | "high" | "max"] ?? effort;
}
