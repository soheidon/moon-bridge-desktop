import { useEffect, useState } from "react";
import type { GatewaySnapshot } from "../types/gateway";
import { DEEPSEEK_FLASH, DEEPSEEK_PRO, type DeepSeekModel, type DeepSeekStatus } from "../types/deepseek";
import type { useDeepSeek } from "../hooks/useDeepSeek";

type DeepSeekState = ReturnType<typeof useDeepSeek>;

export function DeepSeekCard({ snapshot, deepseek, onGatewayWarning }: { snapshot: GatewaySnapshot; deepseek: DeepSeekState; onGatewayWarning?: (warning: string | null) => void }) {
  const [apiKey, setApiKey] = useState("");
  const [showApiKey, setShowApiKey] = useState(false);
  const running = snapshot.state === "running";
  const status = deepseek.status;
  const canSave = !deepseek.saving;

  useEffect(() => {
    onGatewayWarning?.(deepseek.commandError?.gatewayLeftRunning ? deepseek.commandError.message : null);
  }, [deepseek.commandError, onGatewayWarning]);

  async function save() {
    if (!canSave) return;
    const succeeded = await deepseek.configure(apiKey);
    if (succeeded) setApiKey("");
  }

  return (
    <section className="panel deepseek-card" aria-labelledby="deepseek-title">
      <div className="panel-header">
        <div>
          <h2 id="deepseek-title">DeepSeek</h2>
          <span className="panel-subtitle">Anthropic互換ルート</span>
        </div>
        <span className={`deepseek-state ${stateClass(status, running)}`}>{stateLabel(status, running)}</span>
      </div>

      {!running && <p className="deepseek-hint">ゲートウェイ停止中でも編集できます。保存時に必要な間だけ自動起動します。</p>}
      {running && !status && !deepseek.error && <p className="deepseek-hint">設定を読み込んでいます。</p>}
      {deepseek.error && <p className="error-text">{deepseek.error}</p>}
      {deepseek.commandError?.gatewayLeftRunning && <p className="error-text">設定保存に失敗しました。確認のためGatewayは実行したままです。</p>}
      {deepseek.progress && <p className="deepseek-hint">{deepseek.progress.message}</p>}

      <div className="deepseek-fields">
        <label>
          <span>APIキー</span>
          <div className="deepseek-key-field">
            <input
              className="api-key-input deepseek-key-input"
              type={showApiKey ? "text" : "password"}
              value={apiKey}
              placeholder={status?.apiKeySet ? "設定済み（変更時のみ入力）" : "sk-..."}
              onChange={(event) => setApiKey(event.target.value)}
              disabled={deepseek.saving}
              autoComplete="off"
            />
            <button type="button" className="btn btn-secondary" onClick={() => setShowApiKey((current) => !current)} disabled={deepseek.saving}>
              {showApiKey ? "隠す" : "表示"}
            </button>
          </div>
        </label>
        <fieldset disabled={deepseek.saving}>
          <legend>使用モデル</legend>
          {([DEEPSEEK_PRO, DEEPSEEK_FLASH] as DeepSeekModel[]).map((option) => (
            <label className="deepseek-model-option" key={option}>
              <input type="radio" name="deepseek-model" checked={deepseek.model === option} onChange={() => deepseek.setModel(option)} />
              <span><strong>{modelLabel(option)}</strong><small>{option}</small></span>
            </label>
          ))}
        </fieldset>
        <label>
          <span>Reasoning</span>
          <select value={deepseek.reasoningEffort} onChange={(event) => deepseek.setReasoningEffort(event.target.value)} disabled={deepseek.saving || deepseek.reasoningOptions.length === 0}>
            {deepseek.reasoningOptions.map((effort) => <option key={effort} value={effort}>{reasoningLabel(effort)}</option>)}
          </select>
        </label>
      </div>

      <div className="deepseek-route">
        <span>ルート</span>
        <code>{status?.routeAlias ?? "moonbridge"} → {modelLabel(deepseek.model)}</code>
      </div>

      <div className="deepseek-actions">
        <button className="btn btn-primary" disabled={!canSave} onClick={() => void save()}>
          {deepseek.saving ? "保存中…" : "DeepSeekを保存"}
        </button>
        {status?.active && <span className="deepseek-active-note">Codexから利用可能</span>}
        <button className="btn btn-secondary" disabled={deepseek.saving || deepseek.testingConnection} onClick={() => void deepseek.testConnection()}>
          {deepseek.testingConnection ? "接続確認中…" : "接続を確認"}
        </button>
      </div>
      {deepseek.connectionTest && (
        <p className={deepseek.connectionTest.result.ok ? "success-text" : "error-text"}>
          {deepseek.connectionTest.result.message}（{deepseek.connectionTest.result.code}）
        </p>
      )}
    </section>
  );
}

function modelLabel(model: DeepSeekModel) {
  return model === DEEPSEEK_PRO ? "V4 Pro" : "V4 Flash";
}

function reasoningLabel(effort: string) {
  if (effort === "low") return "Low";
  if (effort === "high") return "High";
  if (effort === "max") return "Max";
  return effort;
}

function stateLabel(status: DeepSeekStatus | null, running: boolean) {
  if (!running) return "ゲートウェイ停止中";
  if (status?.active) return "有効";
  if (status?.apiKeySet) return "未選択";
  return "未設定";
}

function stateClass(status: DeepSeekStatus | null, running: boolean) {
  if (!running) return "muted";
  if (status?.active) return "active";
  return "inactive";
}
