import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { save } from "@tauri-apps/plugin-dialog";
import { useCallback, useEffect, useRef, useState } from "react";
import type { TrafficAnalysisStatus, TrafficCommandError, TrafficExportResult, TrafficObservation, TrafficOperation, TrafficProgress } from "../types/trafficAnalysis";

export function useTrafficAnalysis() {
  const [status, setStatus] = useState<TrafficAnalysisStatus | null>(null);
  const [observations, setObservations] = useState<TrafficObservation[]>([]);
  const [progress, setProgress] = useState<TrafficProgress | null>(null);
  const [error, setError] = useState<TrafficCommandError | null>(null);
  const [operationId, setOperationId] = useState<string | null>(null);
  const [pending, setPending] = useState<Partial<Record<TrafficOperation, boolean>>>({});
  const [lastExport, setLastExport] = useState<TrafficExportResult | null>(null);
  const [exitPromptOpen, setExitPromptOpen] = useState(false);
  const lastSequence = useRef(0);
  const operationIds = useRef<Partial<Record<TrafficOperation, string>>>({});
  const pendingRef = useRef<Partial<Record<TrafficOperation, boolean>>>(pending);

  useEffect(() => {
    pendingRef.current = pending;
  }, [pending]);

  const refresh = useCallback(async () => {
    try {
      const next = await invoke<TrafficAnalysisStatus>("traffic_analysis_status");
      setStatus(next);
      if (next.capture.state === "capturing" || next.capture.state === "passthrough" || next.capture.state === "draining" || next.integrationActive) {
        try {
          const page = await invoke<{ observations: TrafficObservation[]; dropped: number; lastSequence: number }>("traffic_analysis_observations", {
            input: { after: lastSequence.current },
          });
          const fresh = page.observations.filter((item) => item.sequence > lastSequence.current);
          if (fresh.length > 0) {
            setObservations((current) => [...current, ...fresh].slice(-2000));
            lastSequence.current = Math.max(lastSequence.current, ...fresh.map((item) => item.sequence));
          }
        } catch {
          // The capture may have just drained; keep the already safe local snapshot.
        }
      }
    } catch (reason) {
      setError(asTrafficError(reason, "traffic_analysis_status"));
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
    let unlisten: UnlistenFn | undefined;
    void listen<TrafficProgress>("traffic-analysis-progress", (event) => {
      if (!disposed && Object.values(operationIds.current).includes(event.payload.operationId)) setProgress(event.payload);
    }).then((fn) => {
      if (disposed) fn();
      else unlisten = fn;
    });
    return () => {
      disposed = true;
      unlisten?.();
    };
  }, []);

  useEffect(() => {
    let disposed = false;
    let unlisten: UnlistenFn | undefined;
    void listen<string>("desktop-exit-confirmation-requested", (event) => {
      if (!disposed && event.payload === "traffic_analysis") setExitPromptOpen(true);
    }).then((fn) => {
      if (disposed) fn();
      else unlisten = fn;
    });
    return () => {
      disposed = true;
      unlisten?.();
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

  const runMutation = useCallback(async <T,>(operation: TrafficOperation, command: string, input: Record<string, unknown>) => {
    const nextOperationId = beginOperation(operation);
    if (!nextOperationId) return null;
    try {
      const result = await invoke<T>(command, { input: { ...input, operationId: nextOperationId } });
      await refresh();
      return result;
    } catch (reason) {
      setError(asTrafficError(reason, command));
      await refresh();
      return null;
    } finally {
      finishOperation(operation);
    }
  }, [beginOperation, finishOperation, refresh]);

  const start = useCallback(() => runMutation("starting", "traffic_analysis_start", {}), [runMutation]);
  const restartCapture = useCallback(() => runMutation("restarting", "traffic_analysis_restart_capture", {}), [runMutation]);
  const stop = useCallback(() => runMutation("stopping", "traffic_analysis_stop", {}), [runMutation]);
  const finishRelay = useCallback((discardUnsaved = false) => {
    if (pendingRef.current.stopping === true) return Promise.resolve(null);
    return runMutation("finalizing", "traffic_analysis_finish_relay", { discardUnsaved });
  }, [runMutation]);
  const retryAutosave = useCallback(() => runMutation("retryingAutosave", "traffic_analysis_retry_autosave", {}), [runMutation]);

  const clear = useCallback(async () => {
    if (!window.confirm("観測一覧をクリアしますか？この操作は現在のメモリ上の観測だけを消去します。")) return;
    const result = await runMutation<TrafficAnalysisStatus>("clearing", "traffic_analysis_clear", {});
    if (result) {
      setObservations([]);
      lastSequence.current = 0;
    }
  }, [runMutation]);

  const exportObservations = useCallback(async () => {
    const operation = "exporting" as const;
    const nextOperationId = beginOperation(operation);
    if (!nextOperationId) return null;
    const stamp = new Date().toISOString().replace(/[.:]/g, "-");
    try {
      const destination = await save({ defaultPath: `moon-bridge-traffic-analysis-${stamp}.log`, filters: [{ name: "Log", extensions: ["log"] }] });
      if (!destination) return null;
      const result = await invoke<TrafficExportResult>("traffic_analysis_export", { input: { operationId: nextOperationId, destination } });
      setLastExport(result);
      await refresh();
      return result;
    } catch (reason) {
      setError(asTrafficError(reason, "traffic_analysis_export"));
      await refresh();
      return null;
    } finally {
      finishOperation(operation);
    }
  }, [beginOperation, finishOperation, refresh]);

  const restore = useCallback((confirmConflict = false) => runMutation("restoring", "traffic_analysis_restore_config", { confirmConflict }), [runMutation]);

  const revealExport = useCallback(() => {
    if (!lastExport) return Promise.resolve(null);
    return runMutation("revealing", "traffic_analysis_reveal_export", { destination: lastExport.destination });
  }, [lastExport, runMutation]);

  const openLogFolder = useCallback(async () => {
    const nextOperationId = beginOperation("openingFolder");
    if (!nextOperationId) return false;
    try {
      await invoke("traffic_analysis_open_log_folder");
      return true;
    } catch (reason) {
      setError(asTrafficError(reason, "traffic_analysis_open_log_folder"));
      return false;
    } finally {
      finishOperation("openingFolder");
    }
  }, [beginOperation, finishOperation]);

  const cancelExit = useCallback(async () => {
    await invoke("desktop_cancel_exit");
    setExitPromptOpen(false);
  }, []);

  const confirmExit = useCallback(async (discardUnsaved = false) => {
    if (status?.integrationActive && !await stop()) return;
    if (!await finishRelay(discardUnsaved)) return;
    await invoke("desktop_confirm_exit");
    setExitPromptOpen(false);
    await getCurrentWindow().close();
  }, [finishRelay, status?.integrationActive, stop]);

  return { status, observations, progress, error, operationId, pending, lastExport, start, restartCapture, stop, finishRelay, retryAutosave, clear, exportObservations, revealExport, openLogFolder, restore, refresh, exitPromptOpen, cancelExit, confirmExit };
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
