export const DEEPSEEK_PRO = "deepseek-v4-pro";
export const DEEPSEEK_FLASH = "deepseek-v4-flash";

export type DeepSeekModel = typeof DEEPSEEK_PRO | typeof DEEPSEEK_FLASH;

export interface DeepSeekModelMetadata {
  id: DeepSeekModel;
  displayName: string;
  allowedReasoningEfforts: string[];
  defaultReasoningEffort: string;
}

export interface DeepSeekMetadata {
  models: DeepSeekModelMetadata[];
}

export interface DeepSeekStatus {
  gatewayRunning: boolean;
  providerExists: boolean;
  apiKeySet: boolean;
  configured: boolean;
  active: boolean;
  selectedModel: DeepSeekModel | null;
  reasoningEffort: string;
  reasoningExplicitlyConfigured: boolean;
  allowedReasoningEfforts: string[];
  routeAlias: string;
}

export interface DeepSeekOperationProgress {
  operationId: string;
  operation: "save" | "connection_test";
  stage: string;
  message: string;
}

export interface DeepSeekSaveResult {
  operationId: string;
  status: DeepSeekStatus;
  gatewaySnapshot: import("./gateway").GatewaySnapshot;
  gatewayLeftRunning: boolean;
  warning: string | null;
}

export interface DeepSeekConnectionTestResult {
  operationId: string;
  result: {
    ok: boolean;
    code: string;
    message: string;
    model: string;
  };
  gatewaySnapshot: import("./gateway").GatewaySnapshot;
  gatewayLeftRunning: boolean;
  warning: string | null;
}

export interface CommandError {
  operation: string;
  stage: string;
  code: string;
  message: string;
  field: string | null;
  retryable: boolean;
  mutationStarted: boolean;
  gatewayLeftRunning: boolean;
  gatewaySnapshot: import("./gateway").GatewaySnapshot | null;
}
