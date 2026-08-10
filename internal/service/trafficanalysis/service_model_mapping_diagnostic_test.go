package trafficanalysis

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

// testServiceWithMapping builds a Service in an arbitrary process-local state
// for diagnostic observation tests. It bypasses the ownership transaction flow
// on purpose: the diagnostic helper is read-only, so each guard must be placed
// independently. proxyState "" leaves the proxy nil.
func testServiceWithMapping(m *modelMapping, mode ManagementMode, ownerID string, gen uint64, gwID, gwAddr, proxyState string) *Service {
	svc := &Service{
		generation:     gen,
		mode:           mode,
		gatewayID:      gwID,
		gatewayAddr:    gwAddr,
		desktopOwnerID: ownerID,
		modelMapping:   m,
	}
	if proxyState != "" {
		p := newFakeProxy(CaptureConfig{})
		p.st.State = proxyState
		svc.proxy = p
	}
	return svc
}

func TestModelMappingDiagnosticHitCase(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	defer svc.CloseCapture(context.Background())
	if _, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1"); err != nil {
		t.Fatalf("ClaimDesktopExpected() error = %v", err)
	}
	if err := svc.SetDesktopModelMappingExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1", "gpt-test", "moonbridge"); err != nil {
		t.Fatalf("SetDesktopModelMappingExpected() error = %v", err)
	}
	if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); !ok || got != "moonbridge" {
		t.Fatalf("lazy bind = %q/%v, want moonbridge/true", got, ok)
	}
	want := modelMappingDiag{
		mappingPresent:    triTrue,
		sourceState:       triBound,
		sourceModelMatch:  triTrue,
		generationMatch:   triTrue,
		gatewayMatch:      triTrue,
		relayActive:       triTrue,
		ownerMatch:        triTrue,
		lazyBindAttempted: triNA,
		lazyBindSuccess:   triNA,
	}
	if got := svc.modelMappingDiagnosticLocked("gpt-test"); got != want {
		t.Fatalf("diagnostic = %+v, want %+v", got, want)
	}
}

func TestModelMappingDiagnosticAbsentMappingIsNA(t *testing.T) {
	svc := testServiceWithMapping(nil, ModeIdle, "", 0, "", "", "")
	want := modelMappingDiag{
		mappingPresent:    triFalse,
		sourceState:       triNA,
		sourceModelMatch:  triNA,
		generationMatch:   triNA,
		gatewayMatch:      triNA,
		relayActive:       triNA,
		ownerMatch:        triNA,
		lazyBindAttempted: triNA,
		lazyBindSuccess:   triNA,
	}
	if got := svc.modelMappingDiagnosticLocked("gpt-test"); got != want {
		t.Fatalf("diagnostic = %+v, want %+v", got, want)
	}
}

func TestModelMappingDiagnosticGuardBreaks(t *testing.T) {
	base := &modelMapping{
		sourceModel:    "gpt-test",
		sourceBound:    true,
		targetRoute:    "moonbridge",
		generation:     3,
		gatewayID:      "gateway-1",
		gatewayAddress: "127.0.0.1:38440",
		ownerID:        "owner-1",
	}

	tests := []struct {
		name    string
		m       *modelMapping
		mode    ManagementMode
		ownerID string
		gen     uint64
		gwID    string
		gwAddr  string
		proxy   string
		query   string
		want    modelMappingDiag
	}{
		{
			name: "source mismatch", m: base, mode: ModeDesktop, ownerID: "owner-1", gen: 3, gwID: "gateway-1", gwAddr: "127.0.0.1:38440", proxy: "capturing", query: "other-model",
			want: modelMappingDiag{mappingPresent: triTrue, sourceState: triBound, sourceModelMatch: triFalse, generationMatch: triTrue, gatewayMatch: triTrue, relayActive: triTrue, ownerMatch: triTrue, lazyBindAttempted: triNA, lazyBindSuccess: triNA},
		},
		{
			name: "generation mismatch", m: base, mode: ModeDesktop, ownerID: "owner-1", gen: 4, gwID: "gateway-1", gwAddr: "127.0.0.1:38440", proxy: "capturing", query: "gpt-test",
			want: modelMappingDiag{mappingPresent: triTrue, sourceState: triBound, sourceModelMatch: triTrue, generationMatch: triFalse, gatewayMatch: triTrue, relayActive: triTrue, ownerMatch: triTrue, lazyBindAttempted: triNA, lazyBindSuccess: triNA},
		},
		{
			name: "gateway mismatch", m: base, mode: ModeDesktop, ownerID: "owner-1", gen: 3, gwID: "gateway-2", gwAddr: "127.0.0.1:38440", proxy: "capturing", query: "gpt-test",
			want: modelMappingDiag{mappingPresent: triTrue, sourceState: triBound, sourceModelMatch: triTrue, generationMatch: triTrue, gatewayMatch: triFalse, relayActive: triTrue, ownerMatch: triTrue, lazyBindAttempted: triNA, lazyBindSuccess: triNA},
		},
		{
			name: "relay not active", m: base, mode: ModeDesktop, ownerID: "owner-1", gen: 3, gwID: "gateway-1", gwAddr: "127.0.0.1:38440", proxy: "stopped", query: "gpt-test",
			want: modelMappingDiag{mappingPresent: triTrue, sourceState: triBound, sourceModelMatch: triTrue, generationMatch: triTrue, gatewayMatch: triTrue, relayActive: triFalse, ownerMatch: triTrue, lazyBindAttempted: triNA, lazyBindSuccess: triNA},
		},
		{
			name: "proxy absent", m: base, mode: ModeDesktop, ownerID: "owner-1", gen: 3, gwID: "gateway-1", gwAddr: "127.0.0.1:38440", proxy: "", query: "gpt-test",
			want: modelMappingDiag{mappingPresent: triTrue, sourceState: triBound, sourceModelMatch: triTrue, generationMatch: triTrue, gatewayMatch: triTrue, relayActive: triFalse, ownerMatch: triTrue, lazyBindAttempted: triNA, lazyBindSuccess: triNA},
		},
		{
			name: "owner mismatch", m: base, mode: ModeDesktop, ownerID: "owner-2", gen: 3, gwID: "gateway-1", gwAddr: "127.0.0.1:38440", proxy: "capturing", query: "gpt-test",
			want: modelMappingDiag{mappingPresent: triTrue, sourceState: triBound, sourceModelMatch: triTrue, generationMatch: triTrue, gatewayMatch: triTrue, relayActive: triTrue, ownerMatch: triFalse, lazyBindAttempted: triNA, lazyBindSuccess: triNA},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := testServiceWithMapping(tt.m, tt.mode, tt.ownerID, tt.gen, tt.gwID, tt.gwAddr, tt.proxy)
			if got := svc.modelMappingDiagnosticLocked(tt.query); got != tt.want {
				t.Fatalf("diagnostic = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestModelMappingDiagnosticOwnerIsNAOutsideDesktop(t *testing.T) {
	base := &modelMapping{sourceModel: "gpt-test", sourceBound: true, targetRoute: "moonbridge", generation: 3, gatewayID: "gateway-1", gatewayAddress: "127.0.0.1:38440", ownerID: "owner-1"}
	svc := testServiceWithMapping(base, ModeCaptureOnly, "owner-1", 3, "gateway-1", "127.0.0.1:38440", "passthrough")
	want := modelMappingDiag{mappingPresent: triTrue, sourceState: triBound, sourceModelMatch: triTrue, generationMatch: triTrue, gatewayMatch: triTrue, relayActive: triTrue, ownerMatch: triNA, lazyBindAttempted: triNA, lazyBindSuccess: triNA}
	if got := svc.modelMappingDiagnosticLocked("gpt-test"); got != want {
		t.Fatalf("diagnostic = %+v, want %+v", got, want)
	}
}

func TestModelMappingForLogIsSecretFree(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	defer svc.CloseCapture(context.Background())
	if _, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1"); err != nil {
		t.Fatalf("ClaimDesktopExpected() error = %v", err)
	}
	if err := svc.SetDesktopModelMappingExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-1", "gpt-test", "moonbridge"); err != nil {
		t.Fatalf("SetDesktopModelMappingExpected() error = %v", err)
	}
	if got, ok := svc.ObservedModelFor("gpt-test", "gateway-1"); !ok || got != "moonbridge" {
		t.Fatalf("lazy bind = %q/%v, want moonbridge/true", got, ok)
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

	got, ok := svc.ModelMappingFor("gpt-test")
	if !ok || got != "moonbridge" {
		t.Fatalf("mapping = %q/%v, want moonbridge/true", got, ok)
	}
	out := buf.String()
	if !strings.Contains(out, "traffic model routing:") {
		t.Fatalf("diagnostic log missing: %q", out)
	}
	for _, secret := range []string{"gpt-test", "moonbridge", "gateway-1", "127.0.0.1:38440", "owner-1"} {
		if strings.Contains(out, secret) {
			t.Fatalf("diagnostic log leaked %q: %q", secret, out)
		}
	}
}
