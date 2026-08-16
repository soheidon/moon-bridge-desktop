import type { useRoutingProfiles } from "../hooks/useRoutingProfiles";
import type { RoutingReasoning, RoutingSlot } from "../types/routingProfile";
import type { RuntimeConfigurationSnapshot, RuntimeSlotSnapshot } from "../types/gateway";

type Props = {
  routing: ReturnType<typeof useRoutingProfiles>;
  runtime?: RuntimeConfigurationSnapshot | null;
};

const REASONING_LABEL: Record<RoutingReasoning, string> = {
  low: "Low",
  high: "High",
  max: "Max",
};

const PROVIDER_CATALOG = ["DeepSeek", "MiMo", "MiniMax", "Kimi", "OpenRouter"];

function placeholderProfile(displayName: string) {
  return {
    id: displayName.toLowerCase(),
    displayName,
    active: false,
    configured: false,
    slots: (["Sol", "Terra", "Luna"] as const).map((name, index) => ({
      id: (["sol", "terra", "luna"] as const)[index],
      displayName: name,
      providerId: displayName.toLowerCase(),
      providerLabel: displayName,
      upstreamModel: "未設定",
    })),
  };
}

function slotSummary(slot: RoutingSlot): string {
  const reasoning = slot.reasoning ? ` + thinking: ${REASONING_LABEL[slot.reasoning]}` : "";
  return `${slot.displayName} → ${slot.upstreamModel}${reasoning}`;
}

function runtimeSlotSummary(slot: RuntimeSlotSnapshot, name: string): string {
  if (slot.state !== "ready") return `${name} → ${slot.state}`;
  const mode = slot.mode === "thinking" ? `Thinking / ${slot.configuredEffort ?? "unknown"}` : "Normal";
  return `${name} → ${slot.provider ?? "unknown"} / ${slot.upstreamModel ?? "unknown"} / ${mode}`;
}

function RuntimeConfigurationBlock({ runtime }: { runtime?: RuntimeConfigurationSnapshot | null }) {
  if (!runtime || runtime.state === "unavailable") {
    return (
      <section className="runtime-configuration-block" aria-label="Gateway effective configuration">
        <h3>Gateway実効設定 / Effective configuration</h3>
        <p className="deepseek-state muted">Gateway停止中または実効設定を確認できません</p>
      </section>
    );
  }
  const statusLabel = runtime.state === "ready" ? "Ready" : runtime.state === "degraded" ? "Degraded" : "Invalid";
  return (
    <section className={`runtime-configuration-block runtime-${runtime.state}`} aria-label="Gateway effective configuration">
      <div className="runtime-configuration-header">
        <h3>Gateway実効設定 / Effective configuration</h3>
        <span>{statusLabel} · {runtime.readySlotCount}/3 ready</span>
      </div>
      <p className="deepseek-state">Credential: {runtime.credentialState}</p>
      <p className="deepseek-state">Routing: {runtime.routingExtensionState} · Active profile: {runtime.activeProfileState}</p>
      <div className="profile-route-list">
        <div className="profile-route"><span className="up-mono">{runtimeSlotSummary(runtime.slots.sol, "Sol")}</span></div>
        <div className="profile-route"><span className="up-mono">{runtimeSlotSummary(runtime.slots.terra, "Terra")}</span></div>
        <div className="profile-route"><span className="up-mono">{runtimeSlotSummary(runtime.slots.luna, "Luna")}</span></div>
      </div>
    </section>
  );
}

// The whole card is the click target and delegates to the shared activateProfile
// (Anthro Bridge tile pattern). Selection moves only after the backend confirms,
// so the UI never holds an optimistic active state.
export function RoutingProfilePanel({ routing, runtime }: Props) {
  const running = routing.gatewayRunning;
  const busy = routing.busy;
  const profiles = routing.profiles.length > 0
    ? routing.profiles
    : PROVIDER_CATALOG.map(placeholderProfile);
  return (
    <section className="provider-selector-section" aria-labelledby="provider-selector-title">
      <RuntimeConfigurationBlock runtime={runtime} />
      <div className="provider-selector-header">
        <h2 id="provider-selector-title">使用するLLMプロバイダ</h2>
        <span className="provider-selector-subtitle">{running ? "Gateway接続中" : "Gateway開始後に切替できます"}</span>
      </div>
      {routing.error && <p className="error-text">{routing.error}</p>}
      <div className="provider-card-grid">
        {profiles.map((profile) => {
          const activating = routing.activatingProfileId === profile.id;
          const isPlaceholder = !routing.profiles.some((item) => item.id === profile.id);
          const disabled = isPlaceholder || !running || busy || profile.active;
          return (
            <button
              key={profile.id}
              className={`provider-card${profile.active ? " selected" : ""}`}
              type="button"
              disabled={disabled}
              onClick={() => void routing.activateProfile(profile.id)}
            >
              <span className="provider-card-topline">
                <strong>{profile.displayName}</strong>
                {profile.active && <span className="provider-card-badge">選択中</span>}
              </span>
              <div className="profile-route-list">
                {profile.slots.map((slot) => (
                  <div className="profile-route" key={slot.id} title={slotSummary(slot)}>
                    <span className="up-mono">{slotSummary(slot)}</span>
                  </div>
                ))}
              </div>
              {activating && <span className="provider-card-progress">切替中…</span>}
            </button>
          );
        })}
      </div>
    </section>
  );
}
