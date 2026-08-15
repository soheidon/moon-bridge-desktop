export type GatewayState = "stopped" | "starting" | "running" | "stopping" | "error";

export type RuntimeConfigurationState = "unavailable" | "ready" | "degraded" | "invalid";
export type RuntimeSlotState = "ready" | "missing" | "invalid" | "reference_unresolved" | "unknown";
export type RuntimeCredentialState = "available" | "missing" | "unavailable" | "unverified" | "unknown";

export interface RuntimeSlotSnapshot {
  state: RuntimeSlotState;
  provider?: string;
  upstreamModel?: string;
  mode?: "normal" | "thinking" | "unknown";
  configuredEffort?: "none" | "low" | "high" | "max" | "unknown";
  credentialState?: RuntimeCredentialState;
}

export interface RuntimeConfigurationSnapshot {
  state: RuntimeConfigurationState;
  serverInstance?: string;
  resolverGeneration: number;
  installSource: string;
  configSource: string;
  resolverPresent: boolean;
  routingExtensionState: string;
  activeProfileState: string;
  readySlotCount: number;
  credentialState: RuntimeCredentialState;
  slots: {
    sol: RuntimeSlotSnapshot;
    terra: RuntimeSlotSnapshot;
    luna: RuntimeSlotSnapshot;
  };
}

export interface GatewaySnapshot {
  state: GatewayState;
  address: string;
  configPath: string;
  pid: number | null;
  instanceId: string | null;
  error: string | null;
  runtimeConfiguration?: RuntimeConfigurationSnapshot | null;
}

export interface GatewayLog {
  stream: "stdout" | "stderr" | "system";
  line: string;
  timestamp: string;
}
