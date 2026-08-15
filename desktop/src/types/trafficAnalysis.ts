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
    responseModel?: string;
    thinking?: string;
    effectiveEffort?: string;
    credentialState?: RuntimeCredentialState;
    direction?: string;
    statusCode?: number;
    exchangeIndex?: number;
    streaming?: boolean;
    resolver?: TrafficResolverDiagnostic;
  };
}

export type RuntimeCredentialState = "available" | "missing" | "unavailable" | "unverified" | "unknown";

export interface TrafficResolverDiagnostic {
  requestedModel?: "known_sol" | "known_terra" | "known_luna" | "unknown";
  serverInstance?: string;
  resolverGeneration?: number;
  resolverPresent: boolean;
  installSource?: string;
  configSource?: string;
  extensionState?: string;
  activeProfileState?: string;
  slotCount: number;
  solState?: string;
  terraState?: string;
  lunaState?: string;
  normalResult?: string;
  resolvedSlot?: "sol" | "terra" | "luna" | "unknown";
  fallbackResult?: string;
  finalStage?: string;
  knownAlias: boolean;
}

export type TrafficRequestRoute = "explicit_route" | "exact_slot" | "fallback" | "not_found" | "unknown";
export type TrafficTransportOutcome = "not_dispatched" | "dispatched" | "response_received" | "forwarded" | "failed" | "unknown";

export interface TrafficRequestSummary {
  requestAlias: string;
  requestedModel: string;
  serverInstance: string;
  resolverGeneration: number;
  resolverState: "ready" | "degraded" | "invalid" | "absent" | "unknown";
  route: TrafficRequestRoute;
  resolvedSlot: "sol" | "terra" | "luna" | "unknown";
  provider: string;
  upstreamModel: string;
  responseModel: string;
  mode: string;
  configuredEffort: string;
  thinking: string;
  effectiveEffort: string;
  credentialState: RuntimeCredentialState;
  attemptCount: number;
  transportOutcome: TrafficTransportOutcome;
  statusClass: "none" | "2xx" | "4xx" | "5xx" | "other";
  multiAttempt: boolean;
}

function summaryState(resolver: TrafficResolverDiagnostic | undefined): TrafficRequestSummary["resolverState"] {
  if (!resolver) return "unknown";
  if (!resolver.resolverPresent) return "absent";
  if (resolver.extensionState === "invalid" || resolver.activeProfileState === "missing" || resolver.activeProfileState === "invalid") return "invalid";
  if (resolver.slotCount >= 3) return "ready";
  return "degraded";
}

function statusClass(status: number | undefined): TrafficRequestSummary["statusClass"] {
  if (!status) return "none";
  if (status >= 200 && status < 300) return "2xx";
  if (status >= 400 && status < 500) return "4xx";
  if (status >= 500 && status < 600) return "5xx";
  return "other";
}

function requestModel(value: string | undefined): string {
  switch (value) {
    case "gpt-5.6-sol":
    case "gpt-5.6-terra":
    case "gpt-5.6-luna":
      return value;
    default:
      return "unknown";
  }
}

function safeSummaryProvider(value: string | undefined): string {
  switch (value) {
    case "deepseek":
    case "minimax":
    case "kimi":
    case "mimo":
    case "openrouter":
    case "anthropic":
    case "openai":
      return value;
    default:
      return "unknown";
  }
}

function safeSummaryModel(value: string | undefined): string {
  switch (value) {
    case "deepseek-v4-flash":
    case "deepseek-v4-pro":
      return value;
    default:
      return "unknown";
  }
}

function gatewayStatusClass(observation: TrafficObservation): TrafficRequestSummary["statusClass"] {
  return statusClass(observation.gatewayEvent?.statusCode ?? observation.statusCode);
}

// summarizeTrafficRequests is a pure view reducer over already sanitized
// observations. It never reads payloads, config, credentials, or raw IDs.
export function summarizeTrafficRequests(observations: TrafficObservation[]): TrafficRequestSummary[] {
  const groups = new Map<string, TrafficObservation[]>();
  for (const observation of observations) {
    const alias = observation.gatewayEvent?.requestAlias ?? observation.requestAlias;
    if (!alias || !alias.startsWith("req#")) continue;
    const group = groups.get(alias) ?? [];
    group.push(observation);
    groups.set(alias, group);
  }
  const summaries: TrafficRequestSummary[] = [];
  for (const [requestAlias, group] of groups) {
    group.sort((a, b) => a.sequence - b.sequence);
    let resolver: TrafficResolverDiagnostic | undefined;
    let requested = "unknown";
    let route: TrafficRequestRoute = "unknown";
    let resolvedSlot: TrafficRequestSummary["resolvedSlot"] = "unknown";
    let serverInstance = "unknown";
    let generation = 0;
    let provider = "unknown";
    let upstreamModel = "unknown";
    let responseModel = "unknown";
    let mode = "unknown";
    let configuredEffort = "none";
    let thinking = "unknown";
    let effectiveEffort = "none";
    let credentialState: RuntimeCredentialState = "unknown";
    let outcome: TrafficTransportOutcome = "not_dispatched";
    let finalStatus: TrafficRequestSummary["statusClass"] = "none";
    let attemptCount = 0;
    for (const observation of group) {
      const event = observation.gatewayEvent;
      if (!event) continue;
      if (event.resolver) {
        resolver = event.resolver;
        serverInstance = event.resolver.serverInstance ?? "unknown";
        generation = event.resolver.resolverGeneration ?? 0;
        const finalStage = event.resolver.finalStage;
        if (finalStage === "explicit_route" || finalStage === "exact_slot" || finalStage === "fallback" || finalStage === "not_found") route = finalStage;
        if (event.resolver.resolvedSlot === "sol" || event.resolver.resolvedSlot === "terra" || event.resolver.resolvedSlot === "luna") resolvedSlot = event.resolver.resolvedSlot;
      }
      if (event.requestedModel) requested = requestModel(event.requestedModel);
      if (event.routingSlot === "sol" || event.routingSlot === "terra" || event.routingSlot === "luna") resolvedSlot = event.routingSlot;
      if (event.provider) provider = safeSummaryProvider(event.provider);
      if (event.upstreamModel) upstreamModel = safeSummaryModel(event.upstreamModel);
      if (event.responseModel) responseModel = safeSummaryModel(event.responseModel);
      if (event.mode) mode = event.mode;
      if (event.configuredEffort) configuredEffort = event.configuredEffort;
      if (event.thinking) thinking = event.thinking;
      if (event.effectiveEffort) effectiveEffort = event.effectiveEffort;
      if (event.credentialState) credentialState = event.credentialState;
      switch (observation.kind) {
        case "provider_request_prepared":
          attemptCount += 1;
          outcome = "dispatched";
          break;
        case "provider_request_dispatched":
          outcome = "dispatched";
          break;
        case "provider_response_received":
          outcome = "response_received";
          break;
        case "provider_response_forwarded":
          outcome = "forwarded";
          break;
      }
      const currentStatus = gatewayStatusClass(observation);
      if (currentStatus !== "none") finalStatus = currentStatus;
    }
    if (attemptCount === 0) {
      attemptCount = group.filter((item) => item.kind === "provider_request_dispatched").length;
    }
    if (route === "unknown" && resolver?.finalStage === "not_found") route = "not_found";
    summaries.push({
      requestAlias, requestedModel: requested, serverInstance, resolverGeneration: generation,
      resolverState: summaryState(resolver), route, resolvedSlot, provider, upstreamModel, responseModel, mode,
      configuredEffort, thinking, effectiveEffort, credentialState, attemptCount,
      transportOutcome: outcome, statusClass: finalStatus, multiAttempt: attemptCount > 1,
    });
  }
  return summaries.sort((a, b) => a.requestAlias.localeCompare(b.requestAlias, undefined, { numeric: true }));
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
