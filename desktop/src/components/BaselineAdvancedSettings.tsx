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

// Rarely-touched global safety reference; collapsed behind an accordion by
// default. 編集 unlocks Provider / 上流モデル (Mode stays fixed Normal).
export function BaselineAdvancedSettings({ routing }: { routing: Routing }) {
  const baseline = routing.routing?.baseline;
  const [expanded, setExpanded] = useState(false);
  const [stage, setStage] = useState<"locked" | "editing">("locked");
  const [provider, setProvider] = useState(baseline?.providerId ?? "deepseek");
  const [upstreamModel, setUpstreamModel] = useState(baseline?.upstreamModel ?? "deepseek-v4-flash");
  const locked = stage === "locked";

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

  const contentId = "baseline-content";

  return (
    <section className="baseline-advanced-settings" aria-labelledby="baseline-advanced-title">
      <button
        type="button"
        className="baseline-summary"
        aria-expanded={expanded}
        aria-controls={contentId}
        onClick={() => setExpanded((current) => !current)}
      >
        <span className="baseline-summary-chevron" aria-hidden="true">{expanded ? "▾" : "▸"}</span>
        <span id="baseline-advanced-title" className="baseline-heading">Baseline（詳細設定）</span>
      </button>
      {expanded && (
        <div id={contentId} className="baseline-row" role="group" aria-label="Baseline設定">
          <select
            className="baseline-select baseline-select-provider"
            aria-label="Baseline プロバイダ"
            value={provider}
            onChange={(event) => setProvider(event.target.value)}
            disabled={locked || routing.saving}
          >
            {optionsFor(PROVIDER_OPTIONS, provider).map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
          </select>
          <select
            className="baseline-select baseline-select-model"
            aria-label="Baseline 上流モデル"
            value={upstreamModel}
            onChange={(event) => setUpstreamModel(event.target.value)}
            disabled={locked || routing.saving}
          >
            {optionsFor(MODEL_OPTIONS, upstreamModel).map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
          </select>
          <span className="routing-editor-mode-label">モード:</span>
          <select className="baseline-select baseline-select-mode" aria-label="Baseline モード" value="normal" disabled>
            <option value="normal">Normal</option>
          </select>
          {locked ? (
            <button type="button" className="btn btn-secondary btn-small" disabled={routing.saving} onClick={unlock}>編集</button>
          ) : (
            <>
              <button
                type="button"
                className="btn btn-primary btn-small"
                disabled={routing.saving || provider.trim() === "" || upstreamModel.trim() === ""}
                onClick={() => void save()}
              >
                保存
              </button>
              <button type="button" className="btn btn-secondary btn-small" disabled={routing.saving} onClick={cancel}>キャンセル</button>
            </>
          )}
          {routing.saving && <span className="deepseek-hint">保存中…</span>}
          {routing.error && <span className="error-text">{routing.error}</span>}
          <span className="baseline-note">内部の安全基準ルート。通常のルーティングには使用されません。</span>
        </div>
      )}
    </section>
  );
}
