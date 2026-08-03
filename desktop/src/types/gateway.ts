export type GatewayState = "stopped" | "starting" | "running" | "stopping" | "error";

export interface GatewaySnapshot {
  state: GatewayState;
  address: string;
  configPath: string;
  pid: number | null;
  instanceId: string | null;
  error: string | null;
}

export interface GatewayLog {
  stream: "stdout" | "stderr" | "system";
  line: string;
  timestamp: string;
}
