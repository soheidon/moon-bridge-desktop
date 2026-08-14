export type CaptureState = "stopped" | "starting" | "capturing" | "passthrough" | "draining" | "failed";
export type TrafficOperation = "starting" | "restarting" | "stopping" | "clearing" | "restoring" | "openingFolder" | "openingFile" | "finalizing";

export const trafficEventCodes = [
  "traffic_backup_created",
  "traffic_route_applied",
  "traffic_analysis_started",
  "traffic_backup_removed",
  "traffic_route_restored",
  "traffic_analysis_stopped",
  "traffic_backup_create_failed",
  "traffic_restore_failed",
  "traffic_cleanup_pending",
  "traffic_recovery_required",
] as const;

export type TrafficEventCode = typeof trafficEventCodes[number];
export type TrafficEventSeverity = "info" | "success" | "warning" | "error";

export interface TrafficRuntimeEvent {
  timestamp: string;
  code: TrafficEventCode;
  severity: TrafficEventSeverity;
}

export function parseTrafficRuntimeEvent(value: unknown): TrafficRuntimeEvent | null {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  const keys = Object.keys(record).sort();
  if (keys.length !== 3 || keys[0] !== "code" || keys[1] !== "severity" || keys[2] !== "timestamp") return null;
  if (typeof record.timestamp !== "string" || Number.isNaN(Date.parse(record.timestamp))) return null;
  if (typeof record.code !== "string" || !trafficEventCodes.includes(record.code as TrafficEventCode)) return null;
  if (record.severity !== "info" && record.severity !== "success" && record.severity !== "warning" && record.severity !== "error") return null;
  return {
    timestamp: record.timestamp,
    code: record.code as TrafficEventCode,
    severity: record.severity,
  };
}

export interface CaptureStatus {
  state: CaptureState;
  sessionId: string | null;
  captureAddress: string;
  upstreamHost: string;
  startedAt: string | null;
  httpRequests: number;
  sseStreams: number;
  websocketConnections: number;
  observationCount: number;
  observationCapacity: number;
  droppedObservations: number;
  droppedBackpressure: number;
  activeHttpRequests: number;
  activeWebsocketConnections: number;
  lastSequence: number;
  lastSafeError: string | null;
}

export interface TrafficAnalysisStatus {
  capture: CaptureStatus;
  configPath: string;
  configExists: boolean;
  integrationActive: boolean;
  relayActive: boolean;
  autoSaveStatus: "active" | "finalized" | "failed" | null;
  recoveryAvailable: boolean;
  appliedOpenaiBaseUrl: string | null;
  recoveryPhase?: string | null;
  reconciliationStatus?: string | null;
  reconciledAt?: string | null;
}

// TrafficObservation mirrors the Wails DTO. Payload observations remain
// secret-free reductions; gatewayEvent contains only validated routing labels.
export interface TrafficObservation {
  sequence: number;
  timestamp: string;
  kind: string;
  requestAlias?: string;
  direction: string;
  transport: string;
  method?: string;
  statusCode?: number;
  payloadKind: string;
  sseEventType?: string;
  contentEncoding?: string;
  payloadShape?: {
    requestModel?: string;
    topLevelFields?: string[];
    topLevelTypes?: Record<string, string>;
    arrayLengths?: Record<string, number>;
    objectFieldCounts?: Record<string, number>;
    inputItemCount?: number;
    inputItemTypeCounts?: Record<string, number>;
    inputRoleCounts?: Record<string, number>;
    hasPreviousResponseId?: boolean;
    inputItemFingerprints?: Array<{
      index: number;
      fields?: string[];
      type?: string;
      role?: string;
      contentCount?: number;
      objectCount?: number;
      arrayCount?: number;
      identifiers?: {
        responseIdAliases?: string[];
        previousResponseIdAliases?: string[];
        itemIdAliases?: string[];
        callIdAliases?: string[];
        conversationIdAliases?: string[];
        otherIdAliases?: string[];
      };
    }>;
    toolCount?: number;
    toolTypes?: string[];
    eventType?: string;
    objectType?: string;
    status?: string;
    shapeTruncated?: boolean;
  };
  identifiers?: {
    responseIdAliases?: string[];
    previousResponseIdAliases?: string[];
    itemIdAliases?: string[];
    callIdAliases?: string[];
    conversationIdAliases?: string[];
    otherIdAliases?: string[];
  };
  usage?: {
    inputTokens?: number;
    outputTokens?: number;
    totalTokens?: number;
    cachedInputTokens?: number;
    reasoningTokens?: number;
  };
  rawPayloadSize: number;
  decodedObservationSize: number;
  decodingStatus: string;
  partial?: boolean;
  truncated?: boolean;
  disposition: string;
  errorClass?: string;
  gatewayEvent?: {
    requestAlias: string;
    requestedModel?: string;
    routingSlot?: string;
    activeProfile?: string;
    provider?: string;
    upstreamModel?: string;
    mode?: string;
    configuredEffort?: string;
    protocol?: string;
    model?: string;
    thinking?: string;
    effectiveEffort?: string;
  };
}

export interface TrafficObservationPage {
  observations: TrafficObservation[];
  dropped: number;
  lastSequence: number;
}

export interface TrafficAnalysisResult {
  operationId: string;
  status: TrafficAnalysisStatus;
  configPath: string;
  restartCodexRequired: boolean;
}

export interface TrafficExportResult {
  operationId: string;
  destination: string;
  observationCount: number;
}

export interface TrafficProgress {
  operationId: string;
  operation: string;
  stage: string;
  message: string;
}

export interface TrafficCommandError {
  operation: string;
  operationId: string;
  stage: string;
  code: string;
  message: string;
  retryable: boolean;
  configChanged: boolean;
  captureRunning: boolean;
  restartCodexRequired: boolean;
}

export type ExitConfirmationPayload = {
  reason?: string; // "traffic_active" | "gateway_active" | "unsaved_observations" | "recovery_required" | 未知
  trafficActive?: boolean;
  gatewayActive?: boolean;
  unsavedObservations?: boolean;
  recoveryRequired?: boolean;
};
