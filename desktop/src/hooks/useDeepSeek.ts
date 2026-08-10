import { useCallback, useEffect, useState } from "react";
import { command, onEvent } from "../platform/desktop";
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

type WailsDeepSeekSnapshot = { deepseek?: DeepSeekStatus };

export function useDeepSeek(snapshot: GatewaySnapshot) {
  const [status, setStatus] = useState<DeepSeekStatus | null>(null);
  const [metadata, setMetadata] = useState<DeepSeekMetadata | null>(null);
  const [model, setModel] = useState<DeepSeekModel>(DEEPSEEK_PRO);
  const [reasoningEffort, setReasoningEffort] = useState("high");
  const [apiKeyEnv, setApiKeyEnv] = useState("DEEPSEEK_API_KEY");
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
    setMetadata({ models: [
      { id: DEEPSEEK_PRO, displayName: "DeepSeek V4 Pro", allowedReasoningEfforts: ["high", "max"], defaultReasoningEffort: "high" },
      { id: DEEPSEEK_FLASH, displayName: "DeepSeek V4 Flash", allowedReasoningEfforts: ["low", "high", "max"], defaultReasoningEffort: "high" },
    ] });
    return () => {
      disposed = true;
    };
  }, []);

  useEffect(() => {
    let disposed = false;
    const unlisten = onEvent<DeepSeekOperationProgress>("deepseek-operation-progress", (payload) => {
      if (!disposed && payload.operationId === operationId) {
        setProgress(payload);
      }
    });
    return () => {
      disposed = true;
      unlisten();
    };
  }, [operationId]);

  const refresh = useCallback(async () => {
    try {
      const snapshot = await command<WailsDeepSeekSnapshot>("LoadDeepSeekSettings");
      const next = snapshot.deepseek;
      if (!next) throw new Error("deepseek status unavailable");
      setStatus(next);
      setApiKeyEnv(next.apiKeyEnv || "DEEPSEEK_API_KEY");
      if (!modelDirty && (next.selectedModel === DEEPSEEK_PRO || next.selectedModel === DEEPSEEK_FLASH)) {
        setModel(next.selectedModel);
      }
      if (!reasoningDirty && next.reasoningEffort) setReasoningEffort(next.reasoningEffort);
      setError(null);
      setCommandError(null);
    } catch (reason) {
      setError(String(reason));
    }
  }, [modelDirty, reasoningDirty]);

  useEffect(() => {
    void refresh();
  }, [refresh, snapshot.address]);

  const configure = useCallback(async (apiKey: string, nextApiKeyEnv?: string) => {
    const nextOperationId = crypto.randomUUID();
    setOperationId(nextOperationId);
    setProgress({ operationId: nextOperationId, operation: "save", stage: "validating_input", message: "設定値を検証しています" });
    setSaving(true);
    setError(null);
    setCommandError(null);
    try {
      const input: Record<string, string> = {
        apiKey: apiKey.trim(),
        defaultModel: model === DEEPSEEK_FLASH ? "flash" : "pro",
        proReasoning: model === DEEPSEEK_PRO ? reasoningEffort : "high",
        flashReasoning: model === DEEPSEEK_FLASH ? reasoningEffort : "high",
      };
      if (nextApiKeyEnv !== undefined) input.apiKeyEnv = nextApiKeyEnv.trim();
      const snapshot = await command<WailsDeepSeekSnapshot>("SaveDeepSeekSettings", input);
      const next = snapshot.deepseek;
      if (!next) throw new Error("deepseek save result unavailable");
      setStatus(next);
      setModelDirty(false);
      setReasoningDirty(false);
      setProgress({ operationId: nextOperationId, operation: "save", stage: "complete", message: "DeepSeek設定を保存しました" });
      return true;
    } catch (reason) {
      setCommandError(asCommandError(reason));
      setError(commandErrorMessage(reason));
      return false;
    } finally {
      setSaving(false);
    }
  }, [apiKeyEnv, model, reasoningEffort]);

  const activateModel = useCallback(async (nextModel: DeepSeekModel) => {
    if (snapshot.state !== "running" || saving || nextModel === model) return false;
    const nextOperationId = crypto.randomUUID();
    setOperationId(nextOperationId);
    setProgress({ operationId: nextOperationId, operation: "save", stage: "activating", message: "プロバイダを切り替えています" });
    setSaving(true);
    setError(null);
    setCommandError(null);
    try {
      const nextSnapshot = await command<WailsDeepSeekSnapshot>("SaveDeepSeekSettings", {
        apiKey: "",
        apiKeyEnv,
        defaultModel: nextModel === DEEPSEEK_FLASH ? "flash" : "pro",
        proReasoning: nextModel === DEEPSEEK_PRO ? reasoningEffort : "high",
        flashReasoning: nextModel === DEEPSEEK_FLASH ? reasoningEffort : "high",
      });
      const next = nextSnapshot.deepseek;
      if (!next) throw new Error("deepseek activation result unavailable");
      setStatus(next);
      setModel(nextModel);
      setModelDirty(false);
      setReasoningDirty(false);
      setProgress({ operationId: nextOperationId, operation: "save", stage: "complete", message: "プロバイダを切り替えました" });
      return true;
    } catch (reason) {
      setCommandError(asCommandError(reason));
      setError(commandErrorMessage(reason));
      return false;
    } finally {
      setSaving(false);
    }
  }, [apiKeyEnv, model, reasoningEffort, saving, snapshot.state]);

  const testConnection = useCallback(async () => {
    const nextOperationId = crypto.randomUUID();
    setOperationId(nextOperationId);
    setProgress({ operationId: nextOperationId, operation: "connection_test", stage: "validating_input", message: "保存済み設定を検証しています" });
    setTestingConnection(true);
    setError(null);
    setCommandError(null);
    try {
      const result = await command<DeepSeekConnectionTestResult>("TestDeepSeekConnection", { operationId: nextOperationId });
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

  const clearKey = useCallback(async () => {
    const nextOperationId = crypto.randomUUID();
    setOperationId(nextOperationId);
    setSaving(true);
    setError(null);
    setCommandError(null);
    try {
      const snapshot = await command<WailsDeepSeekSnapshot>("ClearDeepSeekKey", { operationId: nextOperationId });
      const next = snapshot.deepseek;
      if (!next) throw new Error("deepseek clear result unavailable");
      setStatus(next);
      setApiKeyEnv(next.apiKeyEnv || "DEEPSEEK_API_KEY");
      setModelDirty(false);
      setReasoningDirty(false);
      return true;
    } catch (reason) {
      setCommandError(asCommandError(reason));
      setError(commandErrorMessage(reason));
      return false;
    } finally {
      setSaving(false);
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
    activateModel,
    testConnection,
    clearKey,
    apiKeyEnv,
    setApiKeyEnv,
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
