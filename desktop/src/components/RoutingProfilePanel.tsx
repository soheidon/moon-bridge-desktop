import type { useRoutingProfiles } from "../hooks/useRoutingProfiles";
import type { RoutingReasoning, RoutingSlot } from "../types/routingProfile";

type Props = {
  routing: ReturnType<typeof useRoutingProfiles>;
};

const REASONING_LABEL: Record<RoutingReasoning, string> = {
  low: "Low",
  high: "High",
  max: "Max",
};

const PROVIDER_CATALOG = ["DeepSeek", "MiMo", "MiniMax", "Kimi", "OpenRouter"];

function reasoningLabel(slot: RoutingSlot): string {
  return slot.reasoning ? REASONING_LABEL[slot.reasoning] : "(既定)";
}

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
  const reasoning = slot.reasoning ? ` + thinking: ${reasoningLabel(slot)}` : " (既定)";
  return `${slot.displayName} → ${slot.upstreamModel}${reasoning}`;
}

// The whole card is the click target and delegates to the shared activateProfile
// (Anthro Bridge tile pattern). Selection moves only after the backend confirms,
// so the UI never holds an optimistic active state.
export function RoutingProfilePanel({ routing }: Props) {
  const running = routing.gatewayRunning;
  const busy = routing.busy;
  const profiles = routing.profiles.length > 0
    ? routing.profiles
    : PROVIDER_CATALOG.map(placeholderProfile);
  return (
    <section className="provider-selector-section" aria-labelledby="provider-selector-title">
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
