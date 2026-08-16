package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"moonbridge/internal/service/codexlauncher"
	"moonbridge/internal/service/gateway"
	"moonbridge/internal/service/trafficanalysis"
	"moonbridge/internal/service/traffictransaction"
)

func TestTrafficSnapshotCleanupDetailsAreSecretSafe(t *testing.T) {
	cases := []struct{ name, route, status string }{
		{"applied_pending", "applied", "pending"},
		{"restored_persistence", "restored", "persistence_failed"},
		{"unchanged_delete", "unchanged", "delete_failed"},
		{"applied_clear", "applied", "clear_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := &DesktopSnapshot{RouteMutationResult: tc.route, CleanupStatus: tc.status, CleanupPending: true}
			result := DesktopCommandResult{OK: false, Value: value, Error: &CommandError{Code: "cleanup_failed"}}
			if result.Value == nil {
				t.Fatal("error result lost Value")
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			raw := string(data)
			if !strings.Contains(raw, tc.route) || !strings.Contains(raw, tc.status) {
				t.Fatal("safe cleanup details missing")
			}
			for _, secret := range []string{"BackupID", "password", "Authorization", "api_key", "prompt", "raw body", "SENTINEL"} {
				if strings.Contains(raw, secret) {
					t.Fatalf("secret marker leaked: %q", secret)
				}
			}
		})
	}

	eventData, err := json.Marshal(traffictransaction.Event{
		Timestamp: "2026-08-13T00:00:00Z",
		Code:      traffictransaction.EventBackupCreated,
		Severity:  traffictransaction.EventSeverityInfo,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(eventData, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 {
		t.Fatalf("event field count = %d, want 3", len(fields))
	}
	for _, field := range []string{"timestamp", "code", "severity"} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("event field %q is missing", field)
		}
	}
	rawEvent := string(eventData)
	for _, sentinel := range []string{"BackupID", "backup-file.toml", "C:\\secret", "https://secret.example", "Authorization", "SENTINEL"} {
		if strings.Contains(rawEvent, sentinel) {
			t.Fatalf("event leaked prohibited category %q", sentinel)
		}
	}
}

func TestTrafficBindingUsesSafeEnvelopeAndRejectsWithoutGateway(t *testing.T) {
	app := NewApp(AppOptions{})
	result := app.StartTrafficAnalysis()
	if result.OK || result.Error == nil {
		t.Fatalf("StartTrafficAnalysis() = %#v, want safe failure", result)
	}
	if result.Value != nil {
		t.Fatal("failed command must not return a success value")
	}
	if result.Error.Code != "traffic_gateway_not_running" {
		t.Fatalf("error code = %q, want traffic_gateway_not_running", result.Error.Code)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"Bearer SENTINEL", "owner-secret", "transaction-secret", "C:\\secret", "http://secret.example"} {
		if strings.Contains(string(encoded), sentinel) {
			t.Fatalf("binding leaked sentinel %q: %s", sentinel, encoded)
		}
	}
}

func TestTrafficStatusDoesNotExposeCaptureIdentity(t *testing.T) {
	app := NewApp(AppOptions{})
	result := app.TrafficAnalysisStatus()
	if !result.OK || result.Value == nil || result.Value.TrafficAnalysis == nil {
		t.Fatalf("TrafficAnalysisStatus() = %#v, want safe snapshot", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "127.0.0.1:38441") || strings.Contains(string(encoded), "gateway-1") {
		t.Fatalf("status exposed internal identity/address: %s", encoded)
	}
}

// TestDesktopObservationsStripsSecrets pins the secret-free DTO boundary:
// internal Observation fields that could carry prompts, bodies, responses,
// headers, URL paths/query, API keys, or model/provider names are dropped by
// desktopObservations and never reach the serialized Wails surface.
func TestDesktopObservationsStripsSecrets(t *testing.T) {
	obs := []trafficanalysis.Observation{{
		Sequence:               1,
		Timestamp:              time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		Direction:              trafficanalysis.DirectionClientToUpstream,
		Transport:              trafficanalysis.TransportHTTP,
		Method:                 "POST",
		StatusCode:             200,
		PayloadKind:            trafficanalysis.PayloadJSON,
		RawPayloadSize:         120,
		DecodedObservationSize: 110,
		DecodingStatus:         trafficanalysis.DecodingDecoded,
		Truncated:              true,
		Disposition:            trafficanalysis.DispositionRecorded,
		// Every field below must be stripped at the backend boundary.
		ReceivedHost:        "SENTINEL_HOST",
		ReceivedPath:        "/responses?api_key=SENTINEL_KEY",
		UpstreamHost:        "SENTINEL_UPSTREAM",
		UpstreamPath:        "/SENTINEL_UPSTREAM_PATH",
		QueryParameterNames: []string{"api_key"},
		ContentType:         "application/json",
		ContentEncoding:     "zstd",
		RawPayloadHMAC:      "SENTINEL_HMAC",
		PayloadShape: &trafficanalysis.PayloadShape{
			TopLevelFields:  []string{"input"},
			ModelValue:      "SENTINEL_MODEL",
			ReasoningEffort: "SENTINEL_REASONING",
		},
		Usage:        &trafficanalysis.UsageSummary{InputTokens: intPtr(500), OutputTokens: intPtr(100)},
		Identifiers:  trafficanalysis.IdentifierSummary{ResponseIDAliases: []string{"id#1"}},
		OpaqueFields: []trafficanalysis.OpaqueFieldSummary{{FieldPath: "SENTINEL_FIELD", ValueType: "string", Size: 10, OpaqueContentHMAC: "SENTINEL_OPAQUE"}},
		HeaderSummary: trafficanalysis.HeaderSummary{
			PresentNames:         []string{"authorization", "api-key"},
			UserAgentProduct:     "SENTINEL_AGENT",
			AuthorizationPresent: true,
		},
	}, {
		Sequence:        2,
		Timestamp:       time.Date(2026, 8, 3, 0, 0, 1, 0, time.UTC),
		Direction:       trafficanalysis.DirectionUpstreamToClient,
		Transport:       trafficanalysis.TransportHTTP,
		StatusCode:      200,
		PayloadKind:     trafficanalysis.PayloadEmpty,
		DecodingStatus:  trafficanalysis.DecodingIdentity,
		Disposition:     trafficanalysis.DispositionRecorded,
		GatewayEvent: &trafficanalysis.GatewayEventSummary{
			RequestAlias:  "req#1",
			Provider:      "deepseek",
			UpstreamModel: "deepseek-v4-flash",
			ResponseModel: "deepseek-v4-flash",
			Direction:     trafficanalysis.DirectionUpstreamToClient,
			StatusCode:    200,
			ExchangeIndex: 1,
		},
	}}
	encoded, err := json.Marshal(desktopObservations(obs))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"sequence":1`, `"transport":"http"`, `"method":"POST"`, `"statusCode":200`,
		`"payloadKind":"json"`, `"contentEncoding":"zstd"`, `"rawPayloadSize":120`, `"decodedObservationSize":110`,
		`"decodingStatus":"decoded"`, `"payloadShape"`, `"identifiers"`, `"responseIdAliases"`, `"truncated":true`, `"disposition":"recorded"`,
		`"inputTokens":500`, `"outputTokens":100`,
		`"responseModel":"deepseek-v4-flash"`, `"upstreamModel":"deepseek-v4-flash"`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("DTO missing safe field %s: %s", want, encoded)
		}
	}
	for _, sentinel := range []string{
		"SENTINEL_KEY", "SENTINEL_HOST", "SENTINEL_UPSTREAM", "SENTINEL_MODEL",
		"SENTINEL_REASONING", "SENTINEL_HMAC", "SENTINEL_FIELD",
		"SENTINEL_OPAQUE", "SENTINEL_AGENT", "SENTINEL_TOP_FIELD",
		"api_key", "authorization", "modelValue", "headerSummary", "opaqueFields",
		"opaqueFields", "receivedPath", "sessionId", "connectionId", "requestId",
		"HMAC_RESPONSE_ID", "responseIdHMACs", "itemIdHMACs", "callIdHMACs",
		"input_tokens", "output_tokens", "total_tokens", "cached_tokens", "reasoning_tokens",
	} {
		if strings.Contains(string(encoded), sentinel) {
			t.Fatalf("DTO leaked secret %q: %s", sentinel, encoded)
		}
	}
}

func TestTrafficAnalysisObservationsBindingSafeEmpty(t *testing.T) {
	app := NewApp(AppOptions{})
	result := app.TrafficAnalysisObservations()
	if !result.OK || result.Value == nil {
		t.Fatalf("TrafficAnalysisObservations() = %#v, want safe empty success", result)
	}
	if result.Value.TrafficObservations == nil {
		t.Fatal("TrafficAnalysisObservations() returned a nil list; want a non-nil empty array")
	}
	if len(result.Value.TrafficObservations) != 0 {
		t.Fatalf("TrafficAnalysisObservations() len = %d, want 0", len(result.Value.TrafficObservations))
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "null") || strings.Contains(string(encoded), "SENTINEL") {
		t.Fatalf("empty observations binding must not carry null or secrets, got: %s", encoded)
	}
}

func TestRecoveryBindingsReturnSafeStructuredErrorsWhenUnavailable(t *testing.T) {
	// Scoped to an empty temp profile so the assertions don't depend on the
	// developer's real config.toml / recovery state (which a prior smoke test
	// may have left a record in). With an empty store, restore hits the
	// "load_state" unavailable path and discard requires explicit confirmation.
	app := NewApp(scopedGatewayIntegration(t, AppOptions{}))
	for name, result := range map[string]DesktopCommandResult{
		"restore": app.RestoreRecovery(RestoreRecoveryInput{}),
		"discard": app.DiscardRecovery(DiscardRecoveryInput{}),
	} {
		if result.OK || result.Error == nil {
			t.Fatalf("%s = %#v, want structured safe error", name, result)
		}
	}
}

func TestStartupReconciliationRunsOnceAndDoesNotStartGateway(t *testing.T) {
	gw := newScriptedController(gateway.State{Status: gateway.StatusStopped})
	app := NewApp(AppOptions{Service: gw, EmitEvents: func(string, any) {}})
	app.startup(context.Background())
	app.startup(context.Background())
	if gw.startCalls != 0 {
		t.Fatalf("startup reconciliation started gateway %d times", gw.startCalls)
	}
	app.shutdown(context.Background())
}

func TestShutdownIsSingleFlightAndMutationIsRejectedWhileClosing(t *testing.T) {
	stopEntered := make(chan struct{})
	stopRelease := make(chan struct{})
	codex := &scriptedCodex{stopFn: func(ctx context.Context, reason codexlauncher.StopReason) (codexlauncher.State, error) {
		close(stopEntered)
		select {
		case <-stopRelease:
		case <-ctx.Done():
		}
		return codexlauncher.State{Status: codexlauncher.StatusStopped, StopReason: reason}, nil
	}}
	app := NewApp(AppOptions{Codex: codex, EmitEvents: noopEmit})
	firstDone := make(chan struct{})
	go func() {
		app.shutdown(context.Background())
		close(firstDone)
	}()
	<-stopEntered

	mutation := app.StartTrafficAnalysis()
	if mutation.OK || mutation.Error == nil || mutation.Error.Code != "desktop_app_not_ready" {
		t.Fatalf("mutation during closing = %#v, want desktop_app_not_ready", mutation)
	}
	secondDone := make(chan struct{})
	go func() {
		app.shutdown(context.Background())
		close(secondDone)
	}()
	select {
	case <-secondDone:
		t.Fatal("duplicate shutdown returned before shared cleanup completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(stopRelease)
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first shutdown did not complete")
	}
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("duplicate shutdown did not join shared result")
	}
	if codex.stopCalls != 1 {
		t.Fatalf("codex stop calls = %d, want 1", codex.stopCalls)
	}
}

func TestSafeEventSinkContainsPanics(t *testing.T) {
	var calls int
	var mu sync.Mutex
	app := NewApp(AppOptions{EmitEvents: func(string, any) {
		mu.Lock()
		calls++
		mu.Unlock()
		panic("event sink sentinel")
	}})
	app.safeEmit("test", map[string]any{"safe": true})
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("event calls = %d, want 1", calls)
	}
}

func intPtr(v int) *int { return &v }
