import { useCallback, useEffect, useState } from "react";
import type { GatewaySnapshot } from "../types/gateway";
import type { DeepSeekStatus } from "../types/deepseek";
import type { RoutingModelCapability, RoutingReasoning } from "../types/routingProfile";
import type { useDeepSeek } from "../hooks/useDeepSeek";
import type { useRoutingProfiles } from "../hooks/useRoutingProfiles";
import { RoutingProfileEditor } from "./RoutingProfileEditor";
import { BaselineAdvancedSettings } from "./BaselineAdvancedSettings";

type DeepSeekState = ReturnType<typeof useDeepSeek>;
type RoutingState = ReturnType<typeof useRoutingProfiles>;

export function DeepSeekCard({ snapshot, deepseek, routing }: { snapshot: GatewaySnapshot; deepseek: DeepSeekState; routing: RoutingState }) {
  const [apiKey, setApiKey] = useState("");
  const [apiKeyEnv, setApiKeyEnv] = useState("DEEPSEEK_API_KEY");
  const [apiKeyEnvDirty, setApiKeyEnvDirty] = useState(false);
  // The API key is saved manually via the キーを保存 button; only the env var
  // name auto-saves on blur/Enter. envSavePending records a committed env save.
  const [envSavePending, setEnvSavePending] = useState(false);
  const [showApiKey, setShowApiKey] = useState(false);
  const [expanded, setExpanded] = useState(true);
  const running = snapshot.state === "running";
  const status = deepseek.status;
  const badge = credentialBadge(status);
  useEffect(() => {
    if (!apiKeyEnvDirty && status?.apiKeyEnv) setApiKeyEnv(status.apiKeyEnv);
  }, [apiKeyEnvDirty, status?.apiKeyEnv]);

  const saveEnv = useCallback(async () => {
    if (!status) return;
    if (!apiKeyEnvDirty) {
      setEnvSavePending(false);
      return;
    }
    if (deepseek.saving) return;
    if (!status.apiKeySet) {
      // First-time setup needs the key, which only the save button can send.
      // Clear the request so a later env edit can't auto-save without a key.
      setEnvSavePending(false);
      return;
    }
    const ok = await deepseek.configure("", apiKeyEnv);
    setEnvSavePending(false);
    if (ok) setApiKeyEnvDirty(false);
  }, [status, apiKeyEnvDirty, deepseek.saving, apiKeyEnv, deepseek.configure]);

  // A committed env save is drained when no other save is in flight.
  useEffect(() => {
    if (!envSavePending) return;
    if (!status) return;
    if (!apiKeyEnvDirty) {
      setEnvSavePending(false);
      return;
    }
    if (deepseek.saving) return;
    void saveEnv();
  }, [envSavePending, status, apiKeyEnvDirty, deepseek.saving, saveEnv]);

  const saveKey = useCallback(async () => {
    if (!status) return;
    if (deepseek.saving) return;
    const key = apiKey.trim();
    if (key === "") return;
    const ok = await deepseek.configure(key, apiKeyEnvDirty ? apiKeyEnv : undefined);
    if (ok) {
      setApiKey("");
      setApiKeyEnvDirty(false);
    }
  }, [status, deepseek.saving, apiKey, apiKeyEnv, apiKeyEnvDirty, deepseek.configure]);

  function requestEnvSave() {
    setEnvSavePending(true);
  }

  const contentId = "deepseek-provider-content";

  return (
    <section className="provider-settings-card" aria-labelledby="deepseek-title">
      <div className="deepseek-summary-bar">
        <button
          type="button"
          className="provider-settings-summary deepseek-provider-summary"
          aria-expanded={expanded}
          aria-controls={contentId}
          onClick={() => setExpanded((current) => !current)}
        >
          <span className="provider-summary-chevron" aria-hidden="true">{expanded ? "▾" : "▸"}</span>
          <span className="provider-summary-name" id="deepseek-title">DeepSeek</span>
          <span className="provider-summary-env">{status?.apiKeyEnv || apiKeyEnv}</span>
          <span className={`deepseek-state ${badge.className}`}>{badge.label}</span>
        </button>
        <div className="deepseek-summary-actions">
          {status?.active && <span className="deepseek-active-note">Codexから利用可能</span>}
          <button
            type="button"
            className="btn btn-secondary"
            disabled={!running || deepseek.saving || deepseek.testingConnection}
            title={!running ? "Gateway実行中のみ利用できます" : undefined}
            onClick={() => void deepseek.testConnection()}
          >
            {deepseek.testingConnection ? "接続確認中…" : "接続を確認"}
          </button>
        </div>
      </div>

      {deepseek.connectionTest && (
        <p className={deepseek.connectionTest.result.ok ? "success-text" : "error-text"}>
          {connectionTestMessage(deepseek.connectionTest.result)}（{deepseek.connectionTest.result.code}）
        </p>
      )}

      {expanded && (
        <div id={contentId} className="deepseek-provider-content">
          {running && !status && !deepseek.error && <p className="deepseek-hint">設定を読み込んでいます。</p>}
          {running && status && !status.apiKeySet && !apiKey.trim() && <p className="deepseek-hint">初回設定ではAPI keyを入力してください。</p>}
          {deepseek.error && <p className="error-text">{deepseek.error}</p>}
          {deepseek.commandError?.gatewayLeftRunning && <p className="error-text">設定保存に失敗しました。確認のためGatewayは実行したままです。</p>}
          {deepseek.progress && <p className="deepseek-hint">{deepseek.progress.message}</p>}

          <div className="deepseek-settings-body">
            <div className="deepseek-setting-row">
              <span className="deepseek-setting-label">環境変数名</span>
              <input
                className="deepseek-env-input"
                value={apiKeyEnv}
                onChange={(event) => { setApiKeyEnv(event.target.value); setApiKeyEnvDirty(true); }}
                onBlur={requestEnvSave}
                onKeyDown={(event) => { if (event.key === "Enter") event.currentTarget.blur(); }}
                disabled={deepseek.saving}
              />
            </div>
            <div className="deepseek-setting-row">
              <span className="deepseek-setting-label">APIキー</span>
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
                <button
                  type="button"
                  className="btn btn-primary btn-small"
                  disabled={deepseek.saving || apiKey.trim() === ""}
                  onClick={() => void saveKey()}
                >
                  キーを保存
                </button>
              </div>
            </div>
          </div>

          <RoutingProfileEditor
            routing={routing}
            capabilities={deepseekCapabilities(deepseek)}
            embedded
          />

          <BaselineAdvancedSettings routing={routing} />
        </div>
      )}
    </section>
  );
}

// connectionTestMessage maps the gateway's allowlisted probe code to actionable
// Japanese wording. The gateway never returns a raw upstream error body, so the
// fallback is the safe server-authored message, never a leaked secret.
function connectionTestMessage(result: { ok: boolean; code: string; message: string }): string {
  switch (result.code) {
    case "ok":
      return "接続成功";
    case "credential_unavailable":
      return "APIキーが利用できません。保存済みキーを再入力するか、環境変数を確認してください";
    case "auth_failed":
      return "認証に失敗しました。APIキーが正しいか確認してください";
    case "rate_limited":
      return "レート制限中です。しばらく待ってから再試行してください";
    case "timeout":
      return "接続がタイムアウトしました。Gatewayの状態を確認して再試行してください";
    case "network_error":
      return "ネットワーク接続に失敗しました。接続を確認してください";
    case "model_unavailable":
      return "プローブに使えるモデルがありません。モデル設定を確認してください";
    case "general":
      return "接続確認に失敗しました。しばらくして再試行してください";
    default:
      return "接続確認に失敗しました。しばらくして再試行してください";
  }
}

function credentialBadge(status: DeepSeekStatus | null): { label: string; className: string } {
  if (!status) return { label: "未確認", className: "unknown" };
  switch (status.credentialState) {
    case "unavailable":
      return { label: "APIキーの再入力が必要です", className: "unavailable" };
    case "unverified":
      return { label: "保存済みキー", className: "unverified" };
    case "available":
      if (status.credentialSource === "stored") return { label: "保存済みキー", className: "active" };
      if (status.credentialSource === "environment") return { label: `設定済（${status.apiKeyEnv || "環境変数"}）`, className: "active" };
      return { label: "未設定", className: "inactive" };
    case "missing":
    default:
      return { label: "未設定", className: "inactive" };
  }
}

function deepseekCapabilities(deepseek: DeepSeekState): RoutingModelCapability[] {
  const status = deepseek.status;
  const statusCapabilities = status ? [
    { modelId: status.pro.modelId, supportedReasoning: status.pro.supported as RoutingReasoning[], defaultReasoning: status.pro.reasoning as RoutingReasoning },
    { modelId: status.flash.modelId, supportedReasoning: status.flash.supported as RoutingReasoning[], defaultReasoning: status.flash.reasoning as RoutingReasoning },
  ] : [];
  const metadataCapabilities = deepseek.metadata?.models.map((model) => ({
    modelId: model.id,
    supportedReasoning: model.allowedReasoningEfforts as RoutingReasoning[],
    defaultReasoning: model.defaultReasoningEffort as RoutingReasoning,
  })) ?? [];
  const byModel = new Map(metadataCapabilities.map((capability) => [capability.modelId, capability]));
  for (const capability of statusCapabilities) {
    const metadata = byModel.get(capability.modelId);
    if (!capability.supportedReasoning.length && metadata) {
      byModel.set(capability.modelId, metadata);
    } else {
      byModel.set(capability.modelId, capability);
    }
  }
  return Array.from(byModel.values());
}
