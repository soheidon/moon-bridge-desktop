package trafficanalysis

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func TestObservedModelForPendingStateModel(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	defer svc.CloseCapture(context.Background())
	if _, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1"); err != nil {
		t.Fatalf("ClaimDesktopExpected() error = %v", err)
	}
	// Before registration there is no mapping: nothing can bind.
	if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); ok || got != "" {
		t.Fatalf("ObservedModelFor bound before registration = %q/%v", got, ok)
	}
	if err := svc.SetDesktopModelMappingExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1", "gpt-test", "moonbridge"); err != nil {
		t.Fatalf("SetDesktopModelMappingExpected() error = %v", err)
	}
	// Pending is distinct from nil: exact match does not fire until bound.
	if got, ok := svc.ModelMappingFor("gpt-test"); ok || got != "" {
		t.Fatalf("pending mapping matched early = %q/%v", got, ok)
	}
	// First relay-proven request binds and resolves in the same call.
	if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); !ok || got != "moonbridge" {
		t.Fatalf("first bind = %q/%v, want moonbridge/true", got, ok)
	}
	// Identical follow-up is an exact match.
	if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); !ok || got != "moonbridge" {
		t.Fatalf("second exact match = %q/%v, want moonbridge/true", got, ok)
	}
	// A different model is never rebound.
	if got, ok := svc.ObservedModelFor("other-model", "gateway-1"); ok || got != "" {
		t.Fatalf("different model rebound = %q/%v", got, ok)
	}
	if _, ok := svc.ModelMappingFor("other-model"); ok {
		t.Fatal("different model matched after bind")
	}
}

func TestObservedModelForRequiresRelayProof(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	defer svc.CloseCapture(context.Background())
	if _, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1"); err != nil {
		t.Fatalf("ClaimDesktopExpected() error = %v", err)
	}
	if err := svc.SetDesktopModelMappingExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1", "gpt-test", "moonbridge"); err != nil {
		t.Fatalf("SetDesktopModelMappingExpected() error = %v", err)
	}
	// Missing or wrong marker must not bind even with every other guard satisfied.
	for _, marker := range []string{"", "other-gateway", "gateway-2"} {
		if got, ok := svc.ObservedModelFor("gpt-test", marker); ok || got != "" {
			t.Fatalf("ObservedModelFor(marker=%q) bound without relay proof = %q/%v", marker, got, ok)
		}
	}
	if got, ok := svc.ModelMappingFor("gpt-test"); ok || got != "" {
		t.Fatalf("mapping bound despite bad markers = %q/%v", got, ok)
	}
	// The owning marker still binds afterwards.
	if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); !ok || got != "moonbridge" {
		t.Fatalf("owner marker bind = %q/%v, want moonbridge/true", got, ok)
	}
}

func TestObservedModelForRefusesBindOnGuardMismatch(t *testing.T) {
	pending := func() *modelMapping {
		return &modelMapping{sourceBound: false, targetRoute: "moonbridge", generation: 3, gatewayID: "gateway-1", gatewayAddress: "127.0.0.1:38440", ownerID: "owner-1"}
	}
	tests := []struct {
		name  string
		build func() *Service
	}{
		{name: "owner mismatch", build: func() *Service {
			return testServiceWithMapping(pending(), ModeDesktop, "owner-2", 3, "gateway-1", "127.0.0.1:38440", "capturing")
		}},
		{name: "generation mismatch", build: func() *Service {
			return testServiceWithMapping(pending(), ModeDesktop, "owner-1", 4, "gateway-1", "127.0.0.1:38440", "capturing")
		}},
		{name: "gateway id mismatch", build: func() *Service {
			return testServiceWithMapping(pending(), ModeDesktop, "owner-1", 3, "gateway-2", "127.0.0.1:38440", "capturing")
		}},
		{name: "gateway address mismatch", build: func() *Service {
			return testServiceWithMapping(pending(), ModeDesktop, "owner-1", 3, "gateway-1", "127.0.0.1:48440", "capturing")
		}},
		{name: "proxy absent", build: func() *Service {
			return testServiceWithMapping(pending(), ModeDesktop, "owner-1", 3, "gateway-1", "127.0.0.1:38440", "")
		}},
		{name: "not capturing", build: func() *Service {
			return testServiceWithMapping(pending(), ModeDesktop, "owner-1", 3, "gateway-1", "127.0.0.1:38440", "passthrough")
		}},
		{name: "not desktop mode", build: func() *Service {
			return testServiceWithMapping(pending(), ModeCaptureOnly, "", 3, "gateway-1", "127.0.0.1:38440", "passthrough")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := tt.build()
			if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); ok || got != "" {
				t.Fatalf("ObservedModelFor bound despite %s = %q/%v", tt.name, got, ok)
			}
		})
	}
	// With all guards aligned, the same request binds.
	svc := testServiceWithMapping(pending(), ModeDesktop, "owner-1", 3, "gateway-1", "127.0.0.1:38440", "capturing")
	if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); !ok || got != "moonbridge" {
		t.Fatalf("all-guards bind = %q/%v, want moonbridge/true", got, ok)
	}
}

func TestObservedModelForPassthroughContract(t *testing.T) {
	// Pending + passthrough never lazily binds (lazy bind requires capturing).
	svc := testServiceWithMapping(&modelMapping{sourceBound: false, targetRoute: "moonbridge", generation: 3, gatewayID: "gateway-1", gatewayAddress: "127.0.0.1:38440", ownerID: "owner-1"}, ModeDesktop, "owner-1", 3, "gateway-1", "127.0.0.1:38440", "passthrough")
	if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); ok || got != "" {
		t.Fatalf("pending passthrough bound = %q/%v", got, ok)
	}
	if got, ok := svc.ModelMappingFor("gpt-test"); ok || got != "" {
		t.Fatalf("pending passthrough mapping matched = %q/%v", got, ok)
	}

	// Bound + passthrough still exact-matches the same source (Plan 4n pause
	// contract), without requiring a marker; a different source stays 404.
	bound := &modelMapping{sourceModel: "gpt-test", sourceBound: true, targetRoute: "moonbridge", generation: 3, gatewayID: "gateway-1", gatewayAddress: "127.0.0.1:38440", ownerID: "owner-1"}
	svc = testServiceWithMapping(bound, ModeDesktop, "owner-1", 3, "gateway-1", "127.0.0.1:38440", "passthrough")
	if got, ok := svc.ObservedModelFor("gpt-test", ""); !ok || got != "moonbridge" {
		t.Fatalf("bound passthrough exact match = %q/%v, want moonbridge/true", got, ok)
	}
	if got, ok := svc.ObservedModelFor("other-model", "gateway-1"); ok || got != "" {
		t.Fatalf("bound passthrough wildcard = %q/%v", got, ok)
	}
}

func TestObservedModelForDiagnosticLazyBindFields(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	defer svc.CloseCapture(context.Background())
	if _, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1"); err != nil {
		t.Fatalf("ClaimDesktopExpected() error = %v", err)
	}
	if err := svc.SetDesktopModelMappingExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1", "gpt-test", "moonbridge"); err != nil {
		t.Fatalf("SetDesktopModelMappingExpected() error = %v", err)
	}

	var buf bytes.Buffer
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	}()

	// Failed bind attempt: source stays unbound, lazy fields remain n/a.
	if got, ok := svc.ObservedModelFor("gpt-test", "other-gateway"); ok || got != "" {
		t.Fatalf("unproven request bound = %q/%v", got, ok)
	}
	out := buf.String()
	for _, want := range []string{"source_state=unbound", "source_model_match=n/a", "lazy_bind_attempted=n/a", "lazy_bind_success=n/a"} {
		if !strings.Contains(out, want) {
			t.Fatalf("failed-bind diagnostic missing %q: %q", want, out)
		}
	}
	buf.Reset()

	// Successful bind: lazy fields are true only on the actual bind.
	if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); !ok || got != "moonbridge" {
		t.Fatalf("bind = %q/%v, want moonbridge/true", got, ok)
	}
	out = buf.String()
	for _, want := range []string{"source_state=bound", "source_model_match=true", "lazy_bind_attempted=true", "lazy_bind_success=true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("bind diagnostic missing %q: %q", want, out)
		}
	}
	buf.Reset()

	// Post-bind exact match: bound state, lazy fields back to n/a, secret-free.
	if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); !ok || got != "moonbridge" {
		t.Fatalf("post-bind = %q/%v", got, ok)
	}
	out = buf.String()
	for _, want := range []string{"source_state=bound", "source_model_match=true", "lazy_bind_attempted=n/a", "lazy_bind_success=n/a"} {
		if !strings.Contains(out, want) {
			t.Fatalf("post-bind diagnostic missing %q: %q", want, out)
		}
	}
	for _, secret := range []string{"gpt-test", "moonbridge", "gateway-1", "127.0.0.1:38440", "owner-1"} {
		if strings.Contains(out, secret) {
			t.Fatalf("diagnostic leaked %q: %q", secret, out)
		}
	}
}
