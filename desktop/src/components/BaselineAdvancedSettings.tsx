import { useState } from "react";
import type { useRoutingProfiles } from "../hooks/useRoutingProfiles";
import type { RoutingBaselineInput } from "../types/routingProfile";

type Routing = ReturnType<typeof useRoutingProfiles>;

// The only provider the baseline can currently point at; values are backend
// provider IDs. A value outside this list (e.g. a future provider) is preserved
// via optionsFor so editing an unrelated field never blanks it.
const PROVIDER_OPTIONS = [{ value: "deepseek", label: "DeepSeek" }];

const MODEL_OPTIONS = [
  { value: "deepseek-v4-flash", label: "deepseek-v4-flash" },
  { value: "deepseek-v4-pro", label: "deepseek-v4-pro" },
];

function optionsFor(options: { value: string; label: string }[], current: string): { value: string; label: string }[] {
  if (options.some((option) => option.value === current)) return options;
  return [{ value: current, label: current || "（未設定）" }, ...options];
}

// Baseline is the global, profile-independent reference route. It is observable
// (editable + persistable) but never drives runtime slot resolution, so the
// locked → editing → saved flow is deliberately explicit: editing alone changes
// nothing at runtime; only a successful save persists the edited values.
export function BaselineAdvancedSettings({ routing }: { routing: Routing }) {
  const baseline = routing.routing?.baseline;
  const [stage, setStage] = useState<"locked" | "editing">("locked");
  const [provider, setProvider] = useState(baseline?.providerId ?? "deepseek");
  const [upstreamModel, setUpstreamModel] = useState(baseline?.upstreamModel ?? "deepseek-v4-flash");

  function unlock() {
    setProvider(baseline?.providerId ?? "deepseek");
    setUpstreamModel(baseline?.upstreamModel ?? "deepseek-v4-flash");
    setStage("editing");
  }

  function cancel() {
    setProvider(baseline?.providerId ?? "deepseek");
    setUpstreamModel(baseline?.upstreamModel ?? "deepseek-v4-flash");
    setStage("locked");
  }

  async function save() {
    const input: RoutingBaselineInput = {
      provider: provider.trim(),
      upstreamModel: upstreamModel.trim(),
    };
    if (!input.provider || !input.upstreamModel) return;
    const ok = await routing.saveBaseline(input);
    if (ok) setStage("locked");
  }

  return (
    <section className="baseline-advanced-settings" aria-labelledby="baseline-advanced-title">
      <h3 id="baseline-advanced-title">Baseline（詳細設定）</h3>
      <p className="deepseek-hint">内部の安全基準ルート。通常のルーティングには使用されません。</p>

      {stage === "locked" ? (
        <div className="baseline-summary-row">
          <span className="baseline-row-label">Baseline</span>
          <span className="up-mono">{baseline ? `${baseline.providerLabel} / ${baseline.upstreamModel} / Normal` : "未設定"}</span>
          <button type="button" className="btn btn-secondary btn-small" onClick={unlock}>編集</button>
        </div>
      ) : (
        <div className="baseline-edit" role="group" aria-label="Baseline設定">
          <div className="baseline-edit-row">
            <label className="baseline-field">
              <span className="baseline-field-label">プロバイダ</span>
              <select
                aria-label="Baseline プロバイダ"
                value={provider}
                onChange={(event) => setProvider(event.target.value)}
                disabled={routing.saving}
              >
                {optionsFor(PROVIDER_OPTIONS, provider).map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
              </select>
            </label>
            <label className="baseline-field">
              <span className="baseline-field-label">上流モデル</span>
              <select
                aria-label="Baseline 上流モデル"
                value={upstreamModel}
                onChange={(event) => setUpstreamModel(event.target.value)}
                disabled={routing.saving}
              >
                {optionsFor(MODEL_OPTIONS, upstreamModel).map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
              </select>
            </label>
            <span className="baseline-mode-label">モード: Normal（固定）</span>
          </div>
          <div className="baseline-edit-actions">
            <button
              type="button"
              className="btn btn-primary btn-small"
              disabled={routing.saving || provider.trim() === "" || upstreamModel.trim() === ""}
              onClick={() => void save()}
            >
              保存
            </button>
            <button type="button" className="btn btn-secondary btn-small" disabled={routing.saving} onClick={cancel}>キャンセル</button>
            {routing.saving && <span className="deepseek-hint">保存中…</span>}
            {routing.error && <span className="error-text">{routing.error}</span>}
          </div>
        </div>
      )}
    </section>
  );
}
