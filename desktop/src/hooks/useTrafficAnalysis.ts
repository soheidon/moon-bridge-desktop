import { useCallback, useEffect, useRef, useState } from "react";
import { command, onEvent } from "../platform/desktop";
import type { ExitConfirmationPayload, TrafficAnalysisStatus, TrafficCommandError, TrafficObservation, TrafficOperation, TrafficProgress } from "../types/trafficAnalysis";

type WailsDesktopSnapshot = {
  trafficAnalysis?: {
    mode: string;
    captureState: string;
    relayActive: boolean;
    integrationActive: boolean;
    httpRequests: number;
    sseStreams: number;
    websocketConnections: number;
    observationCount: number;
    observationCapacity: number;
    droppedObservations: number;
    listening: boolean;
    autoSaveStatus?: string;
  };
  recovery?: {
    phase: string;
    reconciliationStatus: string | null;
    recoveryRequired: boolean;
    restoreRequired: boolean;
    conflict: boolean;
  };
  trafficObservations?: TrafficObservation[];
};

// hasRecoveryAvailable mirrors the backend unresolved guard trafficRecoveryWriter.HasUnresolved:
// reconciliation_required / reconciliation_confirmation surface as recoveryRequired, while the
// remaining restart_failed phase is detected directly. A config_conflict live state carries
// recoveryRequired=true.
export function hasRecoveryAvailable(recovery: WailsDesktopSnapshot["recovery"] | null): boolean {
  if (!recovery) return false;
  return recovery.recoveryRequired === true || recovery.phase === "restart_failed";
}

export function toExitPrompt(payload: ExitConfirmationPayload | undefined): ExitConfirmationPayload | null {
  return payload ?? null;
}

function toAutoSaveStatus(raw: string | undefined): "active" | "finalized" | "failed" | null {
  if (raw === "active" || raw === "finalized" || raw === "failed") return raw;
  return null;
}

export function shouldFinishRelay(payload: ExitConfirmationPayload | null | undefined): boolean {
  return payload?.reason === "traffic_active" || payload?.trafficActive === true;
}

export function toTrafficStatus(snapshot: WailsDesktopSnapshot): TrafficAnalysisStatus {
  const traffic = snapshot.trafficAnalysis;
  const captureState = traffic?.captureState ?? "stopped";
  return {
    capture: {
      state: captureState as TrafficAnalysisStatus["capture"]["state"],
      sessionId: null,
      captureAddress: "",
      upstreamHost: "",
      startedAt: null,
      httpRequests: traffic?.httpRequests ?? 0,
      sseStreams: traffic?.sseStreams ?? 0,
      websocketConnections: traffic?.websocketConnections ?? 0,
      observationCount: traffic?.observationCount ?? 0,
      observationCapacity: traffic?.observationCapacity ?? 0,
      droppedObservations: traffic?.droppedObservations ?? 0,
      droppedBackpressure: 0,
      activeHttpRequests: 0,
      activeWebsocketConnections: 0,
      lastSequence: 0,
      lastSafeError: null,
    },
    configPath: "",
    configExists: false,
    integrationActive: traffic?.integrationActive ?? false,
    relayActive: traffic?.relayActive ?? false,
    autoSaveStatus: toAutoSaveStatus(traffic?.autoSaveStatus),
    recoveryAvailable: hasRecoveryAvailable(snapshot.recovery),
    recoveryPhase: snapshot.recovery?.phase ?? null,
    reconciliationStatus: snapshot.recovery?.reconciliationStatus ?? null,
    appliedOpenaiBaseUrl: null,
  };
}

export function useTrafficAnalysis() {
  const [status, setStatus] = useState<TrafficAnalysisStatus | null>(null);
  const [observations, setObservations] = useState<TrafficObservation[]>([]);
  const [progress, setProgress] = useState<TrafficProgress | null>(null);
  const [error, setError] = useState<TrafficCommandError | null>(null);
  const [operationId, setOperationId] = useState<string | null>(null);
  const [pending, setPending] = useState<Partial<Record<TrafficOperation, boolean>>>({});
  const [exitPrompt, setExitPrompt] = useState<ExitConfirmationPayload | null>(null);
  const lastSequence = useRef(0);
  const operationIds = useRef<Partial<Record<TrafficOperation, string>>>({});
  const pendingRef = useRef<Partial<Record<TrafficOperation, boolean>>>(pending);

  useEffect(() => {
    pendingRef.current = pending;
  }, [pending]);

  const refresh = useCallback(async () => {
    try {
      const next = await command<WailsDesktopSnapshot>("TrafficAnalysisStatus");
      setStatus(toTrafficStatus(next));
    } catch (reason) {
      setError(asTrafficError(reason, "traffic_analysis_status"));
    }
    try {
      const obs = await command<WailsDesktopSnapshot>("TrafficAnalysisObservations");
      setObservations(obs.trafficObservations ?? []);
    } catch (reason) {
      setError(asTrafficError(reason, "traffic_analysis_observations"));
    }
  }, []);

  useEffect(() => {
    let disposed = false;
    void refresh();
    const timer = window.setInterval(() => {
      if (!disposed) void refresh();
    }, 1000);
    return () => {
      disposed = true;
      window.clearInterval(timer);
    };
  }, [refresh]);

  useEffect(() => {
    let disposed = false;
    const unlisten = onEvent<TrafficProgress>("traffic-analysis-progress", (payload) => {
      if (!disposed && Object.values(operationIds.current).includes(payload.operationId)) setProgress(payload);
    });
    return () => {
      disposed = true;
      unlisten();
    };
  }, []);

  useEffect(() => {
    let disposed = false;
    const unlisten = onEvent<ExitConfirmationPayload>("desktop-exit-confirmation-requested", (payload) => {
      if (!disposed) setExitPrompt(toExitPrompt(payload));
    });
    return () => {
      disposed = true;
      unlisten();
    };
  }, []);

  const beginOperation = useCallback((operation: TrafficOperation) => {
    if (operationIds.current[operation]) return null;
    const nextOperationId = crypto.randomUUID();
    operationIds.current[operation] = nextOperationId;
    setOperationId(nextOperationId);
    setProgress({ operationId: nextOperationId, operation: "traffic_analysis", stage: "validating", message: "操作を準備しています" });
    setError(null);
    setPending((current) => ({ ...current, [operation]: true }));
    pendingRef.current = { ...pendingRef.current, [operation]: true };
    return nextOperationId;
  }, []);

  const finishOperation = useCallback((operation: TrafficOperation) => {
    delete operationIds.current[operation];
    setPending((current) => ({ ...current, [operation]: false }));
    pendingRef.current = { ...pendingRef.current, [operation]: false };
  }, []);

  const runMutation = useCallback(async <T,>(operation: TrafficOperation, method: string, input: Record<string, unknown>) => {
    const nextOperationId = beginOperation(operation);
    if (!nextOperationId) return null;
    try {
      const result = await command<T>(method, input);
      await refresh();
      return result;
    } catch (reason) {
      setError(asTrafficError(reason, method));
      await refresh();
      return null;
    } finally {
      finishOperation(operation);
    }
  }, [beginOperation, finishOperation, refresh]);

  const start = useCallback(() => runMutation("starting", "StartTrafficAnalysis", {}), [runMutation]);
  const restartCapture = useCallback(() => runMutation("restarting", "RestartTrafficCapture", {}), [runMutation]);
  const stop = useCallback(() => runMutation("stopping", "StopTrafficAnalysis", {}), [runMutation]);
  const finishRelay = useCallback((discardUnsaved = false) => {
    if (pendingRef.current.stopping === true) return Promise.resolve(null);
    return runMutation("finalizing", "FinishTrafficRelay", { discardUnsaved });
  }, [runMutation]);

  const clear = useCallback(async () => {
    if (!window.confirm("観測一覧をクリアしますか？この操作は現在のメモリ上の観測だけを消去します。")) return;
    const result = await runMutation<TrafficAnalysisStatus>("clearing", "ClearTrafficAnalysis", {});
    if (result) {
      setObservations([]);
      lastSequence.current = 0;
    }
  }, [runMutation]);

  const restore = useCallback((confirmConflict = false) => runMutation("restoring", "RestoreRecovery", { confirmConflict }), [runMutation]);

  const finishRelayResolvingConflict = useCallback(async () => {
    if (pendingRef.current.stopping === true) return null;
    if (status?.reconciliationStatus === "config_conflict") {
      if (!window.confirm("Codex設定に競合があります。分析開始前の設定へ復元して終了しますか？")) return null;
      // Finish is fail-closed while the conflict is unresolved, so restore first.
      // The chain is intentionally non-atomic: after a successful restore the relay
      // stays alive (復元済み・中継継続) and the next call only needs to finish.
      if (!(await restore(true))) return null;
    }
    return finishRelay(false);
  }, [finishRelay, restore, status?.reconciliationStatus]);

  const openLogFolder = useCallback(async () => {
    const nextOperationId = beginOperation("openingFolder");
    if (!nextOperationId) return false;
    try {
      await command("TrafficAnalysisOpenLogFolder");
      return true;
    } catch (reason) {
      setError(asTrafficError(reason, "traffic_analysis_open_log_folder"));
      return false;
    } finally {
      finishOperation("openingFolder");
    }
  }, [beginOperation, finishOperation]);

  const openLogFile = useCallback(async () => {
    const nextOperationId = beginOperation("openingFile");
    if (!nextOperationId) return false;
    try {
      await command("TrafficAnalysisOpenLogFile");
      return true;
    } catch (reason) {
      setError(asTrafficError(reason, "traffic_analysis_open_log_file"));
      return false;
    } finally {
      finishOperation("openingFile");
    }
  }, [beginOperation, finishOperation]);

  const cancelExit = useCallback(async () => {
    try {
      await command("CancelExit");
    } catch (reason) {
      setError(asTrafficError(reason, "CancelExit"));
      return;
    }
    setExitPrompt(null);
  }, []);

  const confirmExit = useCallback(async (discardUnsaved = false) => {
    setError(null);
    if (status?.recoveryAvailable) {
      // A pending restore must be resolved before closing; Disable would fail
      // with a restore conflict again. Restore keeps the relay alive on error
      // so the exit stays retryable.
      if (!(await restore(status?.reconciliationStatus === "config_conflict"))) return;
    } else if (status?.integrationActive && !(await stop())) {
      return;
    }
    if (shouldFinishRelay(exitPrompt) && !(await finishRelay(discardUnsaved))) return;
    try {
      await command("ConfirmExit", { confirm: true, discardUnsaved });
    } catch (reason) {
      setError(asTrafficError(reason, "ConfirmExit"));
      return;
    }
    setExitPrompt(null);
  }, [exitPrompt, finishRelay, restore, status?.integrationActive, status?.recoveryAvailable, status?.reconciliationStatus, stop]);

  return { status, observations, progress, error, operationId, pending, start, restartCapture, stop, finishRelay, finishRelayResolvingConflict, clear, openLogFolder, openLogFile, restore, refresh, exitPrompt, cancelExit, confirmExit };
}

function asTrafficError(reason: unknown, operation: string): TrafficCommandError {
  if (typeof reason === "object" && reason !== null && "code" in reason && "message" in reason) return reason as TrafficCommandError;
  return {
    operation,
    operationId: "",
    stage: "unknown",
    code: "unexpected_error",
    message: String(reason),
    retryable: true,
    configChanged: false,
    captureRunning: false,
    restartCodexRequired: false,
  };
}
