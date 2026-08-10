import { useCallback, useEffect, useRef, useState } from "react";
import { closeWindow, command, onEvent, openDialog } from "../platform/desktop";
import type { GatewaySnapshot } from "../types/gateway";
import type { DeepSeekStatus } from "../types/deepseek";
import type {
  CodexCommandError,
  CodexLaunchResult,
  CodexOperationProgress,
  CodexStatus,
} from "../types/codex";

type WailsCodexState = { status: string; pid: number; codexHome: string };
type WailsCodexValue = { codex?: WailsCodexState };

function toCodexStatus(value: WailsCodexValue): CodexStatus {
  return {
    installed: value.codex !== undefined,
    executablePath: null,
    version: null,
    codexHome: value.codex?.codexHome ?? "",
    configPath: "",
    configExists: value.codex !== undefined,
    routeAlias: "moonbridge",
  };
}

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
      const value = await command<WailsCodexValue>("CodexStatus");
      setStatus(value.codex ? toCodexStatus(value) : null);
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
    const unlisten = onEvent<CodexOperationProgress>("codex-operation-progress", (payload) => {
      if (!disposed && payload.operationId === operationIdRef.current) {
        setProgress(payload);
      }
    });
    return () => {
      disposed = true;
      unlisten();
    };
  }, []);

  useEffect(() => {
    let disposed = false;
    const unlisten = onEvent("desktop-exit-confirmation-requested", () => {
      if (!disposed) setExitPromptOpen(true);
    });
    return () => {
      disposed = true;
      unlisten();
    };
  }, []);

  const chooseProject = useCallback(async () => {
    const selected = await openDialog({ directory: true, multiple: false, title: "Codexプロジェクトフォルダを選択" });
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
      const value = await command<WailsCodexValue>("LaunchCodex", { projectDirectory: projectDirectory.trim() });
      const result: CodexLaunchResult = {
        operationId: nextOperationId,
        terminalPid: value.codex?.pid ?? 0,
        projectDirectory: projectDirectory.trim(),
        codexHome: value.codex?.codexHome ?? "",
        configPath: "",
        codexVersion: "",
        gatewayStartedByOperation: false,
        gatewaySnapshot: snapshot,
        warning: null,
      };
      setLastLaunch(result);
      setProgress({ operationId: nextOperationId, operation: "launch", stage: "complete", message: "Codexを起動しました" });
      return true;
    } catch (reason) {
      setError(asCodexError(reason, "codex_launch"));
      return false;
    } finally {
      setLaunching(false);
    }
  }, [launching, projectDirectory]);

  const cancelExit = useCallback(async () => {
    await command("CancelExit");
    setExitPromptOpen(false);
  }, []);

  const confirmExit = useCallback(async () => {
    await command("ConfirmExit", { confirm: true });
    setExitPromptOpen(false);
    await closeWindow();
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
