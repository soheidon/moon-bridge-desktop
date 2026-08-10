import { useCallback, useEffect, useState } from "react";
import { command } from "../platform/desktop";
import type { GatewaySnapshot } from "../types/gateway";
import type { CommandError } from "../types/deepseek";
import type { RoutingProfileInput, RoutingProfileSnapshot } from "../types/routingProfile";

type WailsRoutingProfileSnapshot = { routingProfiles?: RoutingProfileSnapshot };

export function useRoutingProfiles(snapshot: GatewaySnapshot) {
  const [routing, setRouting] = useState<RoutingProfileSnapshot | null>(null);
  const [activatingProfileId, setActivatingProfileId] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [commandError, setCommandError] = useState<CommandError | null>(null);

  // routing_profiles.config.active_profile の読み込み。Gateway 稼働時は live load、
  // 停止時は persisted config から読み出す（backend が自動分岐）。
  const refresh = useCallback(async () => {
    try {
      const next = await command<WailsRoutingProfileSnapshot>("LoadRoutingProfiles");
      if (!next.routingProfiles) throw new Error("routing profiles unavailable");
      setRouting(next.routingProfiles);
      setError(null);
      setCommandError(null);
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
  const saveProfileDetailed = useCallback(async (input: RoutingProfileInput): Promise<{ ok: boolean; snapshot: RoutingProfileSnapshot | null }> => {
    if (saving) return { ok: false, snapshot: null };
    setSaving(true);
    setError(null);
    setCommandError(null);
    try {
      const next = await command<WailsRoutingProfileSnapshot>("SaveRoutingProfile", {
        profile: input,
      });
      if (!next.routingProfiles) throw new Error("routing profiles unavailable");
      setRouting(next.routingProfiles);
      return { ok: true, snapshot: next.routingProfiles };
    } catch (reason) {
      setCommandError(asCommandError(reason));
      setError(asCommandErrorMessage(reason));
      return { ok: false, snapshot: null };
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
    refresh,
    activateProfile,
    saveProfile,
    saveProfileDetailed,
  };
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
