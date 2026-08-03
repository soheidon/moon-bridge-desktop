import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import { useCallback, useEffect, useState } from "react";
import type { GatewaySnapshot } from "../types/gateway";
import {
  DEEPSEEK_FLASH,
  DEEPSEEK_PRO,
  type DeepSeekMetadata,
  type DeepSeekModel,
  type DeepSeekOperationProgress,
  type DeepSeekConnectionTestResult,
  type CommandError,
  type DeepSeekSaveResult,
  type DeepSeekStatus,
} from "../types/deepseek";

export function useDeepSeek(snapshot: GatewaySnapshot) {
  const [status, setStatus] = useState<DeepSeekStatus | null>(null);
  const [metadata, setMetadata] = useState<DeepSeekMetadata | null>(null);
  const [model, setModel] = useState<DeepSeekModel>(DEEPSEEK_PRO);
  const [reasoningEffort, setReasoningEffort] = useState("high");
  const [modelDirty, setModelDirty] = useState(false);
  const [reasoningDirty, setReasoningDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [progress, setProgress] = useState<DeepSeekOperationProgress | null>(null);
  const [operationId, setOperationId] = useState<string | null>(null);
  const [commandError, setCommandError] = useState<CommandError | null>(null);
  const [connectionTest, setConnectionTest] = useState<DeepSeekConnectionTestResult | null>(null);
  const [testingConnection, setTestingConnection] = useState(false);

  useEffect(() => {
    let disposed = false;
    void invoke<DeepSeekMetadata>("deepseek_metadata")
      .then((next) => {
        if (!disposed) setMetadata(next);
      })
      .catch((reason) => {
        if (!disposed) setError(String(reason));
      });
    return () => {
      disposed = true;
    };
  }, []);

  useEffect(() => {
    let disposed = false;
    let unlisten: UnlistenFn | undefined;
    void listen<DeepSeekOperationProgress>("deepseek-operation-progress", (event) => {
      if (!disposed && event.payload.operationId === operationId) {
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
  }, [operationId]);

  const refresh = useCallback(async () => {
    if (snapshot.state !== "running") {
      setError(null);
      return;
    }
    try {
      const next = await invoke<DeepSeekStatus>("deepseek_status");
      setStatus(next);
      if (!modelDirty && (next.selectedModel === DEEPSEEK_PRO || next.selectedModel === DEEPSEEK_FLASH)) {
        setModel(next.selectedModel);
      }
      if (!reasoningDirty && next.reasoningEffort) setReasoningEffort(next.reasoningEffort);
      setError(null);
      setCommandError(null);
    } catch (reason) {
      setError(String(reason));
    }
  }, [modelDirty, reasoningDirty, snapshot.state]);

  useEffect(() => {
    void refresh();
  }, [refresh, snapshot.address]);

  const configure = useCallback(async (apiKey: string) => {
    const nextOperationId = crypto.randomUUID();
    setOperationId(nextOperationId);
    setProgress({ operationId: nextOperationId, operation: "save", stage: "validating_input", message: "設定値を検証しています" });
    setSaving(true);
    setError(null);
    setCommandError(null);
    try {
      const result = await invoke<DeepSeekSaveResult>("deepseek_save", {
        input: { operationId: nextOperationId, apiKey: apiKey.trim() || null, model, reasoningEffort },
      });
      setStatus(result.status);
      setModelDirty(false);
      setReasoningDirty(false);
      setProgress({ operationId: nextOperationId, operation: "save", stage: "complete", message: result.warning ?? "DeepSeek設定を保存しました" });
      return true;
    } catch (reason) {
      setCommandError(asCommandError(reason));
      setError(commandErrorMessage(reason));
      return false;
    } finally {
      setSaving(false);
    }
  }, [model, reasoningEffort]);

  const testConnection = useCallback(async () => {
    const nextOperationId = crypto.randomUUID();
    setOperationId(nextOperationId);
    setProgress({ operationId: nextOperationId, operation: "connection_test", stage: "validating_input", message: "保存済み設定を検証しています" });
    setTestingConnection(true);
    setError(null);
    setCommandError(null);
    try {
      const result = await invoke<DeepSeekConnectionTestResult>("deepseek_test_connection", {
        input: { operationId: nextOperationId },
      });
      setConnectionTest(result);
      setProgress({ operationId: nextOperationId, operation: "connection_test", stage: "complete", message: result.warning ?? result.result.message });
      return result;
    } catch (reason) {
      setCommandError(asCommandError(reason));
      setError(commandErrorMessage(reason));
      return null;
    } finally {
      setTestingConnection(false);
    }
  }, []);

  const selectModel = useCallback((next: DeepSeekModel) => {
    setModel(next);
    setModelDirty(true);
    const modelMetadata = metadata?.models.find((item) => item.id === next);
    if (!reasoningDirty && modelMetadata && !modelMetadata.allowedReasoningEfforts.includes(reasoningEffort)) {
      setReasoningEffort(modelMetadata.defaultReasoningEffort);
    }
  }, [metadata, reasoningDirty, reasoningEffort]);

  const selectReasoningEffort = useCallback((next: string) => {
    setReasoningEffort(next);
    setReasoningDirty(true);
  }, []);

  const reasoningOptions = status?.allowedReasoningEfforts.length
    ? status.allowedReasoningEfforts
    : metadata?.models.find((item) => item.id === model)?.allowedReasoningEfforts ?? [];

  return {
    status,
    metadata,
    model,
    setModel: selectModel,
    reasoningEffort,
    setReasoningEffort: selectReasoningEffort,
    reasoningOptions,
    saving,
    error,
    progress,
    operationId,
    commandError,
    connectionTest,
    testingConnection,
    hasUnsavedChanges: modelDirty || reasoningDirty,
    refresh,
    configure,
    testConnection,
  };
}

function asCommandError(reason: unknown): CommandError | null {
  if (typeof reason === "object" && reason !== null && "code" in reason && "message" in reason) {
    return reason as CommandError;
  }
  return null;
}

function commandErrorMessage(reason: unknown) {
  const structured = asCommandError(reason);
  return structured ? `${structured.message} (${structured.code})` : String(reason);
}
