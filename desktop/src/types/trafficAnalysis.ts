export type CaptureState = "stopped" | "starting" | "capturing" | "passthrough" | "draining" | "failed";
export type TrafficOperation = "starting" | "restarting" | "stopping" | "exporting" | "clearing" | "restoring" | "revealing" | "openingFolder" | "finalizing" | "retryingAutosave";

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
  recoveryAvailable: boolean;
  appliedOpenaiBaseUrl: string | null;
  recoveryPhase?: string | null;
  reconciliationStatus?: string | null;
  reconciledAt?: string | null;
  autoSave: TrafficAutoSaveStatus;
}

export interface TrafficAutoSaveStatus {
  enabled: boolean;
  active: boolean;
  destination: string | null;
  lastPersistedSequence: number;
  observationsWritten: number;
  sequenceGaps: number;
  lastSyncedAt: string | null;
  finalized: boolean;
  lastSafeError: {
    code: string;
    message: string;
    retryable: boolean;
  } | null;
}

export interface TrafficObservation {
  sequence: number;
  sessionId: string;
  timestamp: string;
  direction: string;
  transport: string;
  method?: string;
  receivedPath?: string;
  upstreamPath?: string;
  statusCode?: number;
  contentType?: string;
  contentEncoding?: string;
  websocketMessageType?: string;
  sseEventType?: string;
  payloadKind: string;
  rawPayloadSize: number;
  decodedObservationSize: number;
  decodingStatus: string;
  payloadShape?: {
    topLevelFields?: string[];
    modelValue?: string;
    reasoningEffort?: string;
    eventType?: string;
    objectType?: string;
    status?: string;
    toolCount?: number;
    shapeTruncated?: boolean;
  };
  identifiers: {
    responseIdHmacs?: string[];
    itemIdHmacs?: string[];
    callIdHmacs?: string[];
    conversationIdHmacs?: string[];
    otherIdHmacs?: string[];
  };
  opaqueFields?: Array<{ fieldPath: string; valueType: string; size: number }>;
  headerSummary: {
    presentNames?: string[];
    authorizationPresent?: boolean;
    cookiePresent?: boolean;
    setCookiePresent?: boolean;
    userAgentProduct?: string;
  };
  truncated?: boolean;
  partial?: boolean;
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
