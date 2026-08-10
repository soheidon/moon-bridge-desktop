import { useCallback, useEffect, useState } from "react";
import { command, onEvent, type CommandError } from "../platform/desktop";
import type { GatewayLog, GatewaySnapshot } from "../types/gateway";

// GatewayStatus/StartGateway/StopGateway resolve to a flat GatewaySnapshot,
// not a nested { gateway: ... } wrapper. Reading value.gateway would always be
// undefined and pin the UI to "stopped".
type WailsGatewayValue = {
  state?: string;
  address?: string;
  configPath?: string;
  pid?: number | null;
  instanceId?: string | null;
  error?: string | null;
};

export function appendLog(current: GatewayLog[], next: GatewayLog, cap = 500): GatewayLog[] {
  if (cap <= 0) return [];
  return [...current.slice(-(cap - 1)), next];
}

export function toGatewaySnapshot(value: WailsGatewayValue): GatewaySnapshot {
  return {
    state: (value.state ?? "stopped") as GatewaySnapshot["state"],
    address: value.address ?? "",
    configPath: value.configPath ?? "",
    pid: value.pid ?? null,
    instanceId: value.instanceId ?? null,
    error: value.error ?? null,
  };
}

function asGatewayError(reason: unknown, operation: string): CommandError {
  if (typeof reason === "object" && reason !== null && "code" in reason && "message" in reason) {
    return reason as CommandError;
  }
  return {
    operation,
    stage: "native",
    code: "native_command_failed",
    message: "Desktop command failed",
    field: null,
    retryable: false,
    mutationStarted: false,
    gatewayLeftRunning: false,
    gatewaySnapshot: null,
  };
}

// The gateway's actual state is authoritative. A start that failed at the
// command level but still brought the gateway up is a success; a start that
// reported success while the gateway never reached running is a mismatch.
export function resolveStartError(startError: unknown, state: GatewaySnapshot["state"]): CommandError | null {
  if (state === "running") return null;
  if (startError !== null) return asGatewayError(startError, "gateway.start");
  return {
    ...asGatewayError(null, "gateway.start"),
    code: "gateway_start_state_mismatch",
    message: "Gatewayがrunning状態に遷移しませんでした",
  };
}

export function resolveStopError(stopError: unknown, state: GatewaySnapshot["state"]): CommandError | null {
  if (state !== "running") return null;
  if (stopError !== null) return asGatewayError(stopError, "gateway.stop");
  return {
    ...asGatewayError(null, "gateway.stop"),
    code: "gateway_stop_state_mismatch",
    message: "Gatewayがまだ実行中です",
  };
}

export function useGateway() {
  const [snapshot, setSnapshot] = useState<GatewaySnapshot | null>(null);
  const [logs, setLogs] = useState<GatewayLog[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<CommandError | null>(null);

  const readStatus = useCallback(async (): Promise<GatewaySnapshot> => {
    const value = await command<WailsGatewayValue>("GatewayStatus");
    const next = toGatewaySnapshot(value);
    setSnapshot(next);
    return next;
  }, []);

  // refresh() never auto-clears error: after a failed stop the gateway is still
  // running, so a running check would erase the stop error on the next
  // gateway-log refresh. Errors are only cleared by the next start()/stop().
  const refresh = useCallback(async () => {
    try {
      await readStatus();
    } catch (reason) {
      setError(asGatewayError(reason, "gateway.status"));
    }
  }, [readStatus]);

  useEffect(() => {
    let disposed = false;
    const unlisten = onEvent<GatewayLog>("gateway-log", (payload) => {
      if (disposed) return;
      setLogs((current) => appendLog(current, payload));
      void refresh();
    });
    void refresh();
    return () => {
      disposed = true;
      unlisten();
    };
  }, [refresh]);

  const start = useCallback(async () => {
    setBusy(true);
    setError(null);
    try {
      let startError: unknown = null;
      try {
        await command<WailsGatewayValue>("StartGateway", { configPath: "" });
      } catch (reason) {
        startError = reason;
      }
      // Re-read the authoritative state; surface the start error only when the
      // gateway did not actually come up.
      try {
        setError(resolveStartError(startError, (await readStatus()).state));
      } catch (reason) {
        setError(startError !== null ? asGatewayError(startError, "gateway.start") : asGatewayError(reason, "gateway.status"));
      }
    } finally {
      setBusy(false);
    }
  }, [readStatus]);

  const stop = useCallback(async () => {
    setBusy(true);
    setError(null);
    try {
      let stopError: unknown = null;
      try {
        await command<WailsGatewayValue>("StopGateway", {});
      } catch (reason) {
        stopError = reason;
      }
      try {
        setError(resolveStopError(stopError, (await readStatus()).state));
      } catch (reason) {
        setError(stopError !== null ? asGatewayError(stopError, "gateway.stop") : asGatewayError(reason, "gateway.status"));
      }
    } finally {
      setBusy(false);
    }
  }, [readStatus]);

  const openConfigFolder = useCallback(() => {
    void command("OpenGatewayConfigFolder");
  }, []);

  return { snapshot, logs, busy, error, refresh, start, stop, openConfigFolder };
}
