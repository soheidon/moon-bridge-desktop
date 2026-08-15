import { describe, expect, it } from "vitest";
import { summarizeTrafficRequests, type TrafficObservation } from "./trafficAnalysis";

function observation(sequence: number, kind: string, event: NonNullable<TrafficObservation["gatewayEvent"]>): TrafficObservation {
  return {
    sequence,
    timestamp: new Date(2026, 0, 1, 0, 0, sequence).toISOString(),
    kind,
    requestAlias: event.requestAlias,
    direction: "gateway",
    transport: "http",
    payloadKind: "event",
    rawPayloadSize: 0,
    decodedObservationSize: 0,
    decodingStatus: "not_applicable",
    disposition: "observed",
    gatewayEvent: event,
  };
}

describe("summarizeTrafficRequests", () => {
  it("correlates exact, fallback, not-found, and multiple-attempt requests without exposing arbitrary labels", () => {
    const resolver = {
      requestedModel: "known_luna" as const,
      serverInstance: "server#1",
      resolverGeneration: 4,
      resolverPresent: true,
      extensionState: "valid",
      activeProfileState: "present_valid",
      slotCount: 3,
      finalStage: "exact_slot",
      resolvedSlot: "luna" as const,
      knownAlias: true,
    };
    const summaries = summarizeTrafficRequests([
      observation(3, "provider_response_forwarded", { requestAlias: "req#12", resolver, provider: "deepseek", upstreamModel: "deepseek-v4-flash", credentialState: "available", statusCode: 200 }),
      observation(1, "routing_resolution_diagnosed", { requestAlias: "req#12", resolver }),
      observation(2, "provider_request_prepared", { requestAlias: "req#12", resolver, provider: "deepseek", upstreamModel: "deepseek-v4-flash", credentialState: "available" }),
      observation(4, "routing_resolution_diagnosed", { requestAlias: "req#13", resolver: { ...resolver, requestedModel: "unknown", finalStage: "fallback", resolvedSlot: "unknown" }, provider: "untrusted-provider", upstreamModel: "untrusted-model" }),
      observation(5, "provider_request_prepared", { requestAlias: "req#13", provider: "deepseek", upstreamModel: "deepseek-v4-pro", credentialState: "missing" }),
      observation(6, "routing_resolution_diagnosed", { requestAlias: "req#14", resolver: { ...resolver, requestedModel: "unknown", finalStage: "not_found", resolvedSlot: "unknown" } }),
      observation(7, "provider_request_prepared", { requestAlias: "req#12", provider: "deepseek", upstreamModel: "deepseek-v4-pro", credentialState: "available" }),
      observation(8, "provider_response_forwarded", { requestAlias: "req#12", provider: "deepseek", upstreamModel: "deepseek-v4-pro", credentialState: "available", statusCode: 200 }),
      observation(9, "provider_response_model", { requestAlias: "req#12", provider: "deepseek", responseModel: "deepseek-v4-pro" }),
      observation(10, "provider_response_model", { requestAlias: "req#13", provider: "deepseek", responseModel: "untrusted-model" }),
    ]);
    expect(summaries).toHaveLength(3);
    expect(summaries.find((item) => item.requestAlias === "req#12")).toMatchObject({ route: "exact_slot", transportOutcome: "forwarded", attemptCount: 2, multiAttempt: true, upstreamModel: "deepseek-v4-pro", responseModel: "deepseek-v4-pro" });
    expect(summaries.find((item) => item.requestAlias === "req#13")).toMatchObject({ route: "fallback", provider: "deepseek", upstreamModel: "deepseek-v4-pro", credentialState: "missing", responseModel: "unknown" });
    expect(summaries.find((item) => item.requestAlias === "req#14")).toMatchObject({ route: "not_found", transportOutcome: "not_dispatched" });
  });

  it("keeps the provider_response_model value when a later forwarded event is unknown", () => {
    const summaries = summarizeTrafficRequests([
      observation(1, "provider_response_model", { requestAlias: "req#1", responseModel: "deepseek-v4-pro" }),
      observation(2, "provider_response_forwarded", { requestAlias: "req#1", responseModel: "unknown" }),
    ]);
    expect(summaries).toHaveLength(1);
    expect(summaries[0].responseModel).toBe("deepseek-v4-pro");
  });

  it("defaults responseModel to unknown when no provider_response_model event exists", () => {
    const summaries = summarizeTrafficRequests([
      observation(1, "provider_response_forwarded", { requestAlias: "req#1", responseModel: "unknown" }),
    ]);
    expect(summaries).toHaveLength(1);
    expect(summaries[0].responseModel).toBe("unknown");
  });
});
