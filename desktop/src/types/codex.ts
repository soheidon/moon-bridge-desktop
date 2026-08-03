import type { GatewaySnapshot } from "./gateway";

export interface CodexStatus {
  installed: boolean;
  executablePath: string | null;
  version: string | null;
  codexHome: string;
  configPath: string;
  configExists: boolean;
  routeAlias: string;
}

export interface CodexOperationProgress {
  operationId: string;
  operation: "launch";
  stage: string;
  message: string;
}

export interface CodexLaunchResult {
  operationId: string;
  terminalPid: number;
  projectDirectory: string;
  codexHome: string;
  configPath: string;
  codexVersion: string;
  gatewayStartedByOperation: boolean;
  gatewaySnapshot: GatewaySnapshot;
  warning: string | null;
}

export interface CodexCommandError {
  operation: string;
  operationId: string;
  stage: string;
  code: string;
  message: string;
  field: string | null;
  retryable: boolean;
  gatewayStartedByOperation: boolean;
  gatewayLeftRunning: boolean;
  gatewaySnapshot: GatewaySnapshot | null;
}
