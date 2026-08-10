export type CaptureState = "stopped" | "starting" | "capturing" | "passthrough" | "draining" | "failed";
export type TrafficOperation = "starting" | "restarting" | "stopping" | "clearing" | "restoring" | "openingFolder" | "openingFile" | "finalizing";

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

// TrafficObservation is the secret-free Desktop summary of one recorded
// observation. It mirrors the Wails DTO and never carries prompts, bodies,
// responses, headers, URL paths/query, API keys, or model/provider names.
export interface TrafficObservation {
  sequence: number;
  timestamp: string;
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
