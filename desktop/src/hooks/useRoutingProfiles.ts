import { useCallback, useEffect, useState } from "react";
import { command } from "../platform/desktop";
import type { GatewaySnapshot } from "../types/gateway";
import type { CommandError } from "../types/deepseek";
import type { RoutingProfileInput, RoutingProfileSaveResult, RoutingProfileSaveStatus, RoutingProfileSnapshot } from "../types/routingProfile";

type WailsRoutingProfileSnapshot = { routingProfiles?: RoutingProfileSnapshot };

export function useRoutingProfiles(snapshot: GatewaySnapshot) {
  const [routing, setRouting] = useState<RoutingProfileSnapshot | null>(null);
  const [activatingProfileId, setActivatingProfileId] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [commandError, setCommandError] = useState<CommandError | null>(null);
  const [saveStatus, setSaveStatus] = useState<RoutingProfileSaveStatus | null>(null);

  // routing_profiles.config.active_profile の読み込み。Gateway 稼働時は live load、
  // 停止時は persisted config から読み出す（backend が自動分岐）。
  const refresh = useCallback(async () => {
    try {
      const next = await command<WailsRoutingProfileSnapshot>("LoadRoutingProfiles");
      if (!next.routingProfiles) throw new Error("routing profiles unavailable");
      setRouting(next.routingProfiles);
      setError(null);
      setCommandError(null);
      setSaveStatus(null);
    } catch (reason) {
      setError(asCommandErrorMessage(reason));
    }
  }, [snapshot.state]);

  useEffect(() => {
    void refresh();
  }, [refresh, snapshot.address]);

  const activateProfile = useCallback(async (profileId: string) => {
    if (snapshot.state !== "running" || saving) return false;
    setActivatingProfileId(profileId);
    setSaving(true);
    setError(null);
    setCommandError(null);
    try {
      const next = await command<WailsRoutingProfileSnapshot>("ActivateProfile", {
        profileId,
      });
      if (!next.routingProfiles) throw new Error("routing profiles unavailable");
      setRouting(next.routingProfiles);
      return true;
    } catch (reason) {
      setCommandError(asCommandError(reason));
      setError(asCommandErrorMessage(reason));
      return false;
    } finally {
      setActivatingProfileId(null);
      setSaving(false);
    }
  }, [snapshot.state, saving]);

  // saveProfileDetailed returns the canonical backend snapshot on success so
  // callers can re-baseline their local draft onto what the backend actually
  // persisted (e.g. normalized model ids). saveProfile stays a boolean wrapper
  // over it for callers that only need success/failure.
  const saveProfileDetailed = useCallback(async (input: RoutingProfileInput): Promise<RoutingProfileSaveResult> => {
    if (saving) return { ok: false, snapshot: null, status: "state_unknown", error: null };
    setSaving(true);
    setError(null);
    setCommandError(null);
    try {
      const next = await command<WailsRoutingProfileSnapshot>("SaveRoutingProfile", {
        profile: input,
      });
      if (!next.routingProfiles) throw new Error("routing profiles unavailable");
      setRouting(next.routingProfiles);
      const active = next.routingProfiles.profiles.find((profile) => profile.id === input.id)?.active === true;
      const status: RoutingProfileSaveStatus = next.routingProfiles.gatewayRunning && active ? "saved_applied" : "saved_stopped";
      setSaveStatus(status);
      return { ok: true, snapshot: next.routingProfiles, status, error: null };
    } catch (reason) {
      const error = asCommandError(reason);
      setCommandError(error);
      setError(asCommandErrorMessage(reason));
      const readBackRequired = error?.mutationStarted === true || error?.details?.saved === true;
      let readBack: RoutingProfileSnapshot | null = null;
      if (readBackRequired) {
        try {
          const loaded = await command<WailsRoutingProfileSnapshot>("LoadRoutingProfiles");
          readBack = loaded.routingProfiles ?? null;
          if (readBack) setRouting(readBack);
        } catch {
          readBack = null;
        }
      }
      if (!readBackRequired) {
        setSaveStatus("save_failed");
        return { ok: false, snapshot: null, status: "save_failed", error };
      }
      if (!readBack) {
        setSaveStatus("state_unknown");
        return { ok: false, snapshot: null, status: "state_unknown", error };
      }
      const matches = profileMatchesInput(readBack, input);
      const status: RoutingProfileSaveStatus = matches
        ? (readBack.gatewayRunning ? "persisted_not_applied" : "saved_stopped")
        : "save_failed";
      setSaveStatus(status);
      return { ok: false, snapshot: readBack, status, error };
    } finally {
      setSaving(false);
    }
  }, [saving]);

  const saveProfile = useCallback(async (input: RoutingProfileInput) => {
    const result = await saveProfileDetailed(input);
    return result.ok;
  }, [saveProfileDetailed]);

  const activeProfileId = routing?.activeProfileId ? routing.activeProfileId : null;
  const profiles = routing?.profiles ?? [];

  return {
    routing,
    profiles,
    activeProfileId,
    gatewayRunning: routing?.gatewayRunning ?? false,
    activatingProfileId,
    saving,
    busy: saving || activatingProfileId !== null,
    error,
    commandError,
    saveStatus,
    refresh,
    activateProfile,
    saveProfile,
    saveProfileDetailed,
  };
}

function profileMatchesInput(snapshot: RoutingProfileSnapshot, input: RoutingProfileInput): boolean {
  const profile = snapshot.profiles.find((candidate) => candidate.id === input.id);
  if (!profile || profile.displayName !== input.displayName) return false;
  return (Object.keys(input.slots) as Array<keyof typeof input.slots>).every((slotId) => {
    const expected = input.slots[slotId];
    const actual = profile.slots.find((slot) => slot.id === slotId);
    if (!actual || actual.providerId !== expected.provider || actual.upstreamModel !== expected.upstreamModel) return false;
    const expectedMode = expected.mode ?? (expected.reasoning ? "thinking" : "normal");
    const actualMode = actual.mode ?? (actual.reasoning ? "thinking" : "normal");
    if (actualMode !== expectedMode) return false;
    const expectedReasoning = expected.reasoning ?? null;
    const actualReasoning = actual.reasoning ?? null;
    return actualMode === "normal" ? actualReasoning === null : actualReasoning === expectedReasoning;
  });
}

function asCommandError(reason: unknown): CommandError | null {
  if (typeof reason === "object" && reason !== null && "code" in reason && "message" in reason) {
    return reason as CommandError;
  }
  return null;
}

function asCommandErrorMessage(reason: unknown) {
  const structured = asCommandError(reason);
  return structured ? `${structured.message} (${structured.code})` : String(reason);
}
