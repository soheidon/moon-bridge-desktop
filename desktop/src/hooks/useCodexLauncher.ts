import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { open } from "@tauri-apps/plugin-dialog";
import { useCallback, useEffect, useRef, useState } from "react";
import type { GatewaySnapshot } from "../types/gateway";
import type { DeepSeekStatus } from "../types/deepseek";
import type {
  CodexCommandError,
  CodexLaunchResult,
  CodexOperationProgress,
  CodexStatus,
} from "../types/codex";

const PROJECT_STORAGE_KEY = "moon-bridge-desktop.codex.project-directory";

export function useCodexLauncher(snapshot: GatewaySnapshot, deepseekStatus: DeepSeekStatus | null) {
  const [status, setStatus] = useState<CodexStatus | null>(null);
  const [projectDirectory, setProjectDirectory] = useState(() => localStorage.getItem(PROJECT_STORAGE_KEY) ?? "");
  const [progress, setProgress] = useState<CodexOperationProgress | null>(null);
  const [operationId, setOperationId] = useState<string | null>(null);
  const [launching, setLaunching] = useState(false);
  const [error, setError] = useState<CodexCommandError | null>(null);
  const [lastLaunch, setLastLaunch] = useState<CodexLaunchResult | null>(null);
  const [exitPromptOpen, setExitPromptOpen] = useState(false);
  const operationIdRef = useRef<string | null>(null);

  const refreshStatus = useCallback(async () => {
    try {
      setStatus(await invoke<CodexStatus>("codex_status"));
    } catch (reason) {
      setStatus(null);
      setError(asCodexError(reason, "codex_status"));
    }
  }, []);

  useEffect(() => {
    void refreshStatus();
  }, [refreshStatus]);

  useEffect(() => {
    let disposed = false;
    let unlisten: UnlistenFn | undefined;
    void listen<CodexOperationProgress>("codex-operation-progress", (event) => {
      if (!disposed && event.payload.operationId === operationIdRef.current) {
        setProgress(event.payload);
      }
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
    void listen("desktop-exit-confirmation-requested", () => {
      if (!disposed) setExitPromptOpen(true);
    }).then((fn) => {
      if (disposed) fn();
      else unlisten = fn;
    });
    return () => {
      disposed = true;
      unlisten?.();
    };
  }, []);

  const chooseProject = useCallback(async () => {
    const selected = await open({ directory: true, multiple: false, title: "Codexプロジェクトフォルダを選択" });
    if (typeof selected === "string") {
      setProjectDirectory(selected);
      localStorage.setItem(PROJECT_STORAGE_KEY, selected);
    }
  }, []);

  const launch = useCallback(async () => {
    if (!projectDirectory.trim() || launching) return false;
    const nextOperationId = crypto.randomUUID();
    operationIdRef.current = nextOperationId;
    setOperationId(nextOperationId);
    setProgress({ operationId: nextOperationId, operation: "launch", stage: "validating_project", message: "起動準備中…" });
    setLaunching(true);
    setError(null);
    try {
      const result = await invoke<CodexLaunchResult>("codex_launch", {
        input: { operationId: nextOperationId, projectDirectory: projectDirectory.trim() },
      });
      setLastLaunch(result);
      setProgress({ operationId: nextOperationId, operation: "launch", stage: "complete", message: result.warning ?? "Codexを起動しました" });
      return true;
    } catch (reason) {
      setError(asCodexError(reason, "codex_launch"));
      return false;
    } finally {
      setLaunching(false);
    }
  }, [launching, projectDirectory]);

  const cancelExit = useCallback(async () => {
    await invoke("desktop_cancel_exit");
    setExitPromptOpen(false);
  }, []);

  const confirmExit = useCallback(async () => {
    await invoke("desktop_confirm_exit");
    setExitPromptOpen(false);
    await getCurrentWindow().close();
  }, []);

  const effectiveModel = deepseekStatus?.selectedModel ?? null;
  const effectiveReasoning = deepseekStatus?.reasoningEffort ?? null;

  return {
    status,
    projectDirectory,
    setProjectDirectory,
    chooseProject,
    launch,
    refreshStatus,
    launching,
    progress,
    operationId,
    error,
    lastLaunch,
    effectiveModel,
    effectiveReasoning,
    routeAlias: deepseekStatus?.routeAlias ?? "moonbridge",
    gatewayRunning: snapshot.state === "running",
    exitPromptOpen,
    cancelExit,
    confirmExit,
  };
}

function asCodexError(reason: unknown, operation: string): CodexCommandError {
  if (typeof reason === "object" && reason !== null && "code" in reason && "message" in reason) {
    return reason as CodexCommandError;
  }
  return {
    operation,
    operationId: "",
    stage: "unknown",
    code: "unexpected_error",
    message: String(reason),
    field: null,
    retryable: true,
    gatewayStartedByOperation: false,
    gatewayLeftRunning: false,
    gatewaySnapshot: null,
  };
}
