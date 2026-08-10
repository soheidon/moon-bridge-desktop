import { useEffect, useRef, useState } from "react";
import type { useRoutingProfiles } from "../hooks/useRoutingProfiles";
import type { RoutingMode, RoutingModelCapability, RoutingProfileCard, RoutingProfileInput, RoutingReasoning, RoutingSlot, RoutingSlotId, RoutingSlotInput } from "../types/routingProfile";

type Routing = ReturnType<typeof useRoutingProfiles>;

// saveProfileDetailed is optional on the prop so callers that predate it still
// work: the editor falls back to the boolean saveProfile. The hook always
// provides it.
type Props = {
  routing: Omit<Routing, "saveProfileDetailed"> & Partial<Pick<Routing, "saveProfileDetailed">>;
  capabilities?: RoutingModelCapability[];
  embedded?: boolean;
};

function capabilityFor(capabilities: RoutingModelCapability[] | undefined, model: string): RoutingModelCapability {
  return capabilities?.find((capability) => capability.modelId === model) ?? { modelId: model, supportedReasoning: [] };
}

function reasoningOptionsFor(capability: RoutingModelCapability): { value: RoutingReasoning; label: string }[] {
  return capability.supportedReasoning.filter((value): value is RoutingReasoning => value === "low" || value === "high" || value === "max").map((value) => ({ value, label: value[0].toUpperCase() + value.slice(1) }));
}

function defaultReasoningFor(capability: RoutingModelCapability): string {
  const supported = reasoningOptionsFor(capability).map((option) => option.value);
  if (capability.defaultReasoning && supported.includes(capability.defaultReasoning)) return capability.defaultReasoning;
  if (supported.includes("high")) return "high";
  return supported[0] ?? "";
}

const SLOT_IDS: RoutingSlotId[] = ["sol", "terra", "luna"];

// UI-facing model catalog. Labels are human-readable; values are the existing
// backend model IDs.
const MODEL_OPTIONS: { value: string; label: string }[] = [
  { value: "deepseek-v4-flash", label: "deepseek-v4-flash" },
  { value: "deepseek-v4-pro", label: "deepseek-v4-pro" },
];

const REASONING_OPTIONS: { value: string; label: string }[] = [
  { value: "", label: "Default" },
  { value: "low", label: "Low" },
  { value: "high", label: "High" },
  { value: "max", label: "Max" },
];

// "" means no reasoning override (Luna). Kept as a string in the draft so the
// select can represent "no override" without a separate boolean.
type DraftSlot = {
  upstreamModel: string;
  mode: RoutingMode;
  reasoning: string;
};

type Draft = {
  displayName: string;
  slots: Record<RoutingSlotId, DraftSlot>;
};

// modelOptionsFor returns the catalog plus, when the current value is outside
// it (e.g. a legacy non-DeepSeek model), a fallback option preserving the
// existing value so an unrelated UI change never blanks it.
function modelOptionsFor(current: string): { value: string; label: string }[] {
  if (MODEL_OPTIONS.some((option) => option.value === current)) {
    return MODEL_OPTIONS;
  }
  return [{ value: current, label: current || "（未設定）" }, ...MODEL_OPTIONS];
}

function toDraft(slot: RoutingSlot): DraftSlot {
  const mode = slot.mode === "thinking" || slot.reasoning ? "thinking" : "normal";
  return {
    upstreamModel: slot.upstreamModel,
    mode,
    reasoning: mode === "thinking" ? (slot.reasoning ?? "high") : "",
  };
}

function fromDraftSlot(slot: DraftSlot): Omit<RoutingSlotInput, "provider"> {
  return {
    upstreamModel: slot.upstreamModel.trim(),
    mode: slot.mode,
    reasoning: slot.mode === "normal" || slot.reasoning === "" ? null : (slot.reasoning as RoutingReasoning),
  };
}

// providerForSlot maps a slot by id (never by array index) to its backend
// providerId. Empty when the snapshot is missing the slot.
function providerForSlot(profile: RoutingProfileCard, slotId: RoutingSlotId): string {
  return profile.slots.find((slot) => slot.id === slotId)?.providerId ?? "";
}

function fromProfile(profile: RoutingProfileCard): Draft {
  return {
    displayName: profile.displayName,
    slots: {
      sol: toDraft(profile.slots.find((s) => s.id === "sol") ?? emptySlot()),
      terra: toDraft(profile.slots.find((s) => s.id === "terra") ?? emptySlot()),
      luna: toDraft(profile.slots.find((s) => s.id === "luna") ?? emptySlot()),
    },
  };
}

function initialDrafts(profiles: ReturnType<typeof useRoutingProfiles>["profiles"]): Record<string, Draft> {
  const drafts: Record<string, Draft> = {};
  for (const profile of profiles) {
    drafts[profile.id] = fromProfile(profile);
  }
  return drafts;
}

function emptySlot(): RoutingSlot {
  return { id: "sol", displayName: "", providerId: "", providerLabel: "", upstreamModel: "", mode: "normal" };
}

export function RoutingProfileEditor({ routing, capabilities = [], embedded = false }: Props) {
  const profiles = routing.profiles;
  const availableCapabilities = capabilities.length ? capabilities : ((routing as ReturnType<typeof useRoutingProfiles> & { capabilities?: RoutingModelCapability[] }).capabilities ?? []);
  const [drafts, setDrafts] = useState<Record<string, Draft>>(() => initialDrafts(profiles));
  const [selectedId, setSelectedId] = useState<string>(profiles[0]?.id ?? "");
  // Bumping retryNonce re-runs the auto-save effect after a manual retry, and
  // saveFailedId tracks which profile's last attempt failed so the UI can offer
  // the (manual-only) retry button.
  const [retryNonce, setRetryNonce] = useState(0);
  const [saveFailedId, setSaveFailedId] = useState<string | null>(null);
  // Per-profile serialized slots of the last successful save and of the last
  // backend attempt. Auto-save persists a draft that differs from lastSaved;
  // a draft that already matches lastAttempted was sent but failed, so it is
  // never resent as-is (only a manual retry clears the attempt).
  const lastSavedRef = useRef<Record<string, string>>({});
  const lastAttemptedRef = useRef<Record<string, string>>({});

  // Async selection + delete fallback: a valid user selection is never stolen,
  // but when the current id is empty or no longer present (profiles arriving
  // after an async load, a profile deleted on the backend), select the active
  // profile first, then the first profile.
  useEffect(() => {
    if (profiles.length === 0) return;
    if (selectedId && profiles.some((p) => p.id === selectedId)) return;
    const active = profiles.find((p) => p.active);
    setSelectedId((active ?? profiles[0]).id);
  }, [profiles, selectedId]);

  // Baseline every profile's draft as clean on arrival so later refreshes can
  // resync it. The baseline is the backend-confirmed content the draft was
  // initialized from; a draft matching its baseline has no unsaved edits.
  useEffect(() => {
    for (const profile of profiles) {
      if (lastSavedRef.current[profile.id] === undefined) {
        const key = JSON.stringify(fromProfile(profile).slots);
        lastSavedRef.current[profile.id] = key;
        lastAttemptedRef.current[profile.id] = key;
      }
    }
  }, [profiles]);

  // Sync drafts from backend snapshot: add never-seen profiles, prune deleted
  // ones, and re-sync non-dirty drafts (draft matches its last-saved baseline).
  // A dirty draft differs from its baseline — an unsaved edit or a failed save —
  // and is never overwritten, so an edit made while a save is in flight is never
  // clobbered by the response. Adopting a backend change moves the baseline up
  // so the freshly-synced draft stays clean.
  useEffect(() => {
    setDrafts((current) => {
      let next: Record<string, Draft> | null = null;
      const known = new Set(profiles.map((p) => p.id));
      for (const id of Object.keys(current)) {
        if (!known.has(id)) {
          next = { ...(next ?? current) };
          delete next[id];
          delete lastSavedRef.current[id];
          delete lastAttemptedRef.current[id];
        }
      }
      for (const profile of profiles) {
        const existing = current[profile.id];
        const baseline = lastSavedRef.current[profile.id];
        if (!existing) {
          const fresh = fromProfile(profile);
          next = { ...(next ?? current), [profile.id]: fresh };
          const freshKey = JSON.stringify(fresh.slots);
          lastSavedRef.current[profile.id] = freshKey;
          lastAttemptedRef.current[profile.id] = freshKey;
        } else if (baseline !== undefined && JSON.stringify(existing.slots) === baseline) {
          const fresh = fromProfile(profile);
          if (JSON.stringify(fresh.slots) !== JSON.stringify(existing.slots) || fresh.displayName !== existing.displayName) {
            next = { ...(next ?? current), [profile.id]: fresh };
            lastSavedRef.current[profile.id] = JSON.stringify(fresh.slots);
          }
        }
      }
      return next ?? current;
    });
  }, [profiles]);

  const selected = drafts[selectedId];
  const selectedProfile = profiles.find((p) => p.id === selectedId);
  const running = routing.gatewayRunning;
  // Fail-closed: never allow saving a profile whose snapshot can't provide a
  // provider for every slot (would otherwise persist empty provider ids).
  const missingProvider = selectedProfile ? SLOT_IDS.some((slotId) => providerForSlot(selectedProfile, slotId) === "") : false;
  const selectedCapabilities = selected ? Object.fromEntries(SLOT_IDS.map((slotId) => [slotId, capabilityFor(availableCapabilities, selected.slots[slotId].upstreamModel)])) as Record<RoutingSlotId, RoutingModelCapability> : null;
  const serialized = selected ? JSON.stringify(selected.slots) : "";
  const hasPending = selected !== undefined && lastSavedRef.current[selectedId] !== undefined && serialized !== lastSavedRef.current[selectedId];
  // The retry button shows only when the current content is exactly what the
  // last backend attempt sent and that attempt failed — never for a fresh edit.
  const saveFailed = saveFailedId === selectedId && serialized !== lastSavedRef.current[selectedId] && lastAttemptedRef.current[selectedId] === serialized;

  function updateSlot(slotId: RoutingSlotId, field: keyof DraftSlot, value: string) {
    setDrafts((current) => ({
      ...current,
      [selectedId]: {
        ...current[selectedId],
        slots: { ...current[selectedId].slots, [slotId]: { ...current[selectedId].slots[slotId], [field]: value } },
      },
    }));
  }

  function handleRetry() {
    if (!selected) return;
    delete lastAttemptedRef.current[selectedId];
    setSaveFailedId(null);
    setRetryNonce((n) => n + 1);
  }

  // Auto-save: persist the selected draft whenever it differs from the last
  // successful save and was not already sent. A save is held back while the
  // Gateway is stopped, a provider is missing, or another save is running, so
  // a change made during an in-flight save is picked up when saving clears. The
  // manual retry bumps retryNonce and clears the failed attempt, re-running this
  // effect to resend; there is no automatic resend loop.
  useEffect(() => {
    if (!selected || !selectedProfile) return;
    if (lastSavedRef.current[selectedId] === undefined) {
      lastSavedRef.current[selectedId] = serialized;
      lastAttemptedRef.current[selectedId] = serialized;
      return;
    }
    if (!running || missingProvider || routing.saving) return;
    if (serialized === lastSavedRef.current[selectedId]) return;
    if (serialized === lastAttemptedRef.current[selectedId]) return;
    lastAttemptedRef.current[selectedId] = serialized;
    setSaveFailedId(null);
    const slots: Record<RoutingSlotId, RoutingSlotInput> = {
      sol: { ...fromDraftSlot(selected.slots.sol), provider: providerForSlot(selectedProfile, "sol") },
      terra: { ...fromDraftSlot(selected.slots.terra), provider: providerForSlot(selectedProfile, "terra") },
      luna: { ...fromDraftSlot(selected.slots.luna), provider: providerForSlot(selectedProfile, "luna") },
    };
    const input: RoutingProfileInput = { id: selectedId, displayName: selected.displayName, slots };
    void (async () => {
      if (routing.saveProfileDetailed) {
        const result = await routing.saveProfileDetailed(input);
        if (result.ok && result.snapshot) {
          const profile = result.snapshot.profiles.find((p) => p.id === selectedId);
          const canonical = profile ? fromProfile(profile) : null;
          if (canonical) {
            lastSavedRef.current[selectedId] = JSON.stringify(canonical.slots);
            // Rebase the draft onto the canonical snapshot only when it was not
            // edited while the save was in flight; a newer edit stays dirty.
            setDrafts((current) => {
              if (JSON.stringify(current[selectedId]?.slots ?? "") !== serialized) return current;
              return { ...current, [selectedId]: canonical };
            });
            setSaveFailedId(null);
          } else {
            lastSavedRef.current[selectedId] = serialized;
            setSaveFailedId(null);
          }
        } else {
          setSaveFailedId(selectedId);
        }
        return;
      }
      const ok = await routing.saveProfile(input);
      if (ok) {
        lastSavedRef.current[selectedId] = serialized;
        setSaveFailedId(null);
      } else {
        setSaveFailedId(selectedId);
      }
    })();
  }, [serialized, selectedId, selected, selectedProfile, running, missingProvider, routing.saving, routing.saveProfile, routing.saveProfileDetailed, retryNonce]);

  return (
    <section className={embedded ? "routing-editor-embedded" : "panel routing-editor"} aria-labelledby={embedded ? undefined : "routing-editor-title"}>
      {!embedded && <div className="panel-header">
        <div>
          <h2 id="routing-editor-title">モデル設定</h2>
          {profiles.length > 1 ? (
            <label className="routing-editor-profile-select">
              <span>プロファイル</span>
              <select value={selectedId} onChange={(event) => setSelectedId(event.target.value)} disabled={routing.saving}>
                {profiles.map((profile) => <option key={profile.id} value={profile.id}>{profile.displayName}{profile.active ? "（選択中）" : ""}</option>)}
              </select>
            </label>
          ) : (
            selected && <span className="panel-subtitle">{selected.displayName}</span>
          )}
        </div>
        <span className="provider-selector-subtitle">{running ? "Gateway接続中" : "Gateway開始後に保存できます"}</span>
      </div>}

      {embedded && profiles.length > 1 && (
        <label className="routing-editor-profile-select">
          <span>プロファイル</span>
          <select value={selectedId} onChange={(event) => setSelectedId(event.target.value)} disabled={routing.saving}>
            {profiles.map((profile) => <option key={profile.id} value={profile.id}>{profile.displayName}{profile.active ? "（選択中）" : ""}</option>)}
          </select>
        </label>
      )}

      {!running && !embedded && <p className="deepseek-hint">ゲートウェイ停止中です。設定値は編集できますが、保存には先にGatewayを開始してください。</p>}
      {routing.error && <p className="error-text">{routing.error}</p>}
      {routing.commandError?.gatewayLeftRunning && <p className="error-text">設定保存に失敗しました。確認のためGatewayは実行したままです。</p>}

      {profiles.length === 0 ? (
        <p className="deepseek-hint">プロファイルがありません。</p>
      ) : (
        <>
          {selected && (
            <div className="routing-editor-rows" role="group" aria-label="ルーティング設定">
              {SLOT_IDS.map((slotId) => {
                const slot = selected.slots[slotId];
                const label = slotLabel(slotId);
                return (
                  <div className="routing-editor-row" key={slotId}>
                    <span className="routing-editor-row-name">{label}</span>
                    <select
                      className="routing-editor-model-select"
                      aria-label={`${label} 上流モデル`}
                      value={slot.upstreamModel}
                      onChange={(event) => updateSlot(slotId, "upstreamModel", event.target.value)}
                    >
                      {modelOptionsFor(slot.upstreamModel).map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
                    </select>
                    <span className="routing-editor-mode-label">モード:</span>
                    <select
                      className="routing-editor-mode-select"
                      aria-label={`${label} モード`}
                      value={slot.mode}
                      onChange={(event) => {
                        const mode = event.target.value as RoutingMode;
                        const capability = selectedCapabilities?.[slotId] ?? { modelId: slot.upstreamModel, supportedReasoning: [] };
                        updateSlot(slotId, "mode", mode);
                        if (mode === "normal") updateSlot(slotId, "reasoning", "");
                        else if (slot.reasoning === "") updateSlot(slotId, "reasoning", defaultReasoningFor(capability));
                      }}
                      disabled={reasoningOptionsFor(selectedCapabilities?.[slotId] ?? { modelId: slot.upstreamModel, supportedReasoning: [] }).length === 0}
                    >
                      <option value="normal">通常</option>
                      <option value="thinking">Thinking</option>
                    </select>
                    {slot.mode === "thinking" && <>
                      <span className="routing-editor-reasoning-label">推論強度:</span>
                      <select
                        className="routing-editor-reasoning-select"
                        aria-label={`${label} Reasoning`}
                        value={slot.reasoning}
                        onChange={(event) => updateSlot(slotId, "reasoning", event.target.value)}
                      >
                        {reasoningOptionsFor(selectedCapabilities?.[slotId] ?? { modelId: slot.upstreamModel, supportedReasoning: [] }).map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
                      </select>
                    </>}
                  </div>
                );
              })}
            </div>
          )}

          {missingProvider && <p className="error-text">プロファイル設定を読み込めないため保存できません。</p>}

          <div className="deepseek-actions">
            {routing.saving && <span className="deepseek-hint">保存中…</span>}
            {hasPending && !running && <span className="deepseek-hint">Gateway開始後に保存されます。</span>}
            {saveFailed && (
              <button type="button" className="routing-editor-retry" onClick={handleRetry}>保存を再試行</button>
            )}
          </div>
        </>
      )}
    </section>
  );
}

function slotLabel(slotId: RoutingSlotId) {
  return slotId === "sol" ? "Sol" : slotId === "terra" ? "Terra" : "Luna";
}
