package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"moonbridge/internal/config"
	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/provider"
)

// fakeAPI is an in-memory stand-in for the gateway management API. It mirrors
// the real server's masking of a configured provider api_key, requires a
// matching base revision on every mutation, and can inject conflicts and
// transport failures.
type fakeAPI struct {
	mu               sync.Mutex
	revision         int
	resources        []configgraph.Resource
	graphErr         error
	mutateErr        error
	failAt           int // after this many successful mutations the next fails (0 = never)
	mutations        int
	conflictN        int // apply a revision conflict to the next N mutations
	corruptReasoning bool
	creates          int
	patches          int
	baseConflicts    int
	effective        config.FileConfig
	credentialStatus []provider.CredentialInfo
	statusErr        error
	connectionTest   ConnectionTestResult
	connectionTestErr error
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{}
}

func (f *fakeAPI) Graph(context.Context) (configgraph.Graph, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.graphErr != nil {
		return configgraph.Graph{}, f.graphErr
	}
	return f.snapshotGraph(), nil
}

func (f *fakeAPI) Effective(context.Context) (config.FileConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.effective, nil
}

func (f *fakeAPI) CredentialStatus(context.Context) ([]provider.CredentialInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.credentialStatus, nil
}

func (f *fakeAPI) TestProvider(context.Context, string) (ConnectionTestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectionTest, f.connectionTestErr
}

func (f *fakeAPI) CreateResource(_ context.Context, baseRevision string, kind configgraph.ResourceKind, id string, value map[string]any) (configgraph.PatchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutations++
	if f.failAt > 0 && f.mutations > f.failAt {
		return configgraph.PatchResponse{}, f.mutateErr
	}
	if f.conflictN > 0 {
		f.conflictN--
		return configgraph.PatchResponse{Result: configgraph.ResultRevisionConflict}, nil
	}
	if baseRevision != strconv.Itoa(f.revision) {
		f.baseConflicts++
		return configgraph.PatchResponse{Result: configgraph.ResultRevisionConflict}, nil
	}
	storedValue := cloneValue(value).(map[string]any)
	if kind == configgraph.ResourceProviderOffer {
		storedValue = wireRoundTrip(storedValue)
	}
	for i := range f.resources {
		if f.resources[i].Kind == kind && f.resources[i].ID == id {
			f.resources[i].Value = storedValue
			f.revision++
			return f.committed(), nil
		}
	}
	f.resources = append(f.resources, configgraph.Resource{Kind: kind, ID: id, Value: storedValue})
	f.revision++
	return f.committed(), nil
}

func (f *fakeAPI) Patch(_ context.Context, req configgraph.PatchRequest) (configgraph.PatchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutations++
	if f.failAt > 0 && f.mutations > f.failAt {
		return configgraph.PatchResponse{}, f.mutateErr
	}
	if f.conflictN > 0 {
		f.conflictN--
		return configgraph.PatchResponse{Result: configgraph.ResultRevisionConflict}, nil
	}
	if req.BaseRevision != strconv.Itoa(f.revision) {
		f.baseConflicts++
		return configgraph.PatchResponse{Result: configgraph.ResultRevisionConflict}, nil
	}
	for _, op := range req.Changes {
		found := false
		for i := range f.resources {
			if f.resources[i].Kind == op.Kind && f.resources[i].ID == op.ID {
				value := cloneValue(f.resources[i].Value).(map[string]any)
				if f.corruptReasoning && op.Field == "default_reasoning_level" {
					value[op.Field] = "bogus-reasoning"
				} else {
					value[op.Field] = cloneValue(op.Value)
				}
				if op.Kind == configgraph.ResourceProviderOffer {
					value = wireRoundTrip(value)
				}
				f.resources[i].Value = value
				found = true
				break
			}
		}
		if !found {
			return configgraph.PatchResponse{
				Result: configgraph.ResultValidationRejected,
				Errors: []configgraph.FieldError{{Code: "missing_resource"}},
			}, nil
		}
	}
	f.revision++
	return f.committed(), nil
}

func (f *fakeAPI) committed() configgraph.PatchResponse {
	g := f.snapshotGraph()
	return configgraph.PatchResponse{Result: configgraph.ResultCommitted, Revision: g.Revision, Graph: &g}
}

func (f *fakeAPI) snapshotGraph() configgraph.Graph {
	resources := make([]configgraph.Resource, len(f.resources))
	for i, r := range f.resources {
		if r.Value != nil {
			r.Value = cloneValue(r.Value).(map[string]any)
		}
		if r.Kind == configgraph.ResourceProvider && r.ID == ProviderID {
			if k, ok := r.Value["api_key"].(string); ok && k != "" {
				r.Value["api_key"] = apiKeyMasked
			}
		}
		resources[i] = r
	}
	return configgraph.Graph{Revision: strconv.Itoa(f.revision), Resources: resources}
}

func (f *fakeAPI) find(kind configgraph.ResourceKind, id string) *configgraph.Resource {
	for i := range f.resources {
		if f.resources[i].Kind == kind && f.resources[i].ID == id {
			return &f.resources[i]
		}
	}
	return nil
}

func cloneValue(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// wireRoundTrip simulates the real server's offer persistence: the incoming
// snake_case value map is decoded into config.OfferFileConfig (whose custom
// MarshalJSON/omitempty drops zero-valued priority and zero pricing fields) and
// re-encoded as a graph value. Storing offers this way makes the fake faithful
// to the real createResource→BuildGraph round-trip, so a bug that only appears
// on the real wire (e.g. priority:0 omitted) is caught by unit tests too.
func wireRoundTrip(value map[string]any) map[string]any {
	var offer config.OfferFileConfig
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	if err := json.Unmarshal(data, &offer); err != nil {
		return value
	}
	out, err := json.Marshal(offer)
	if err != nil {
		return value
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return value
	}
	return m
}

func cloneResource(r configgraph.Resource) configgraph.Resource {
	if r.Value != nil {
		r.Value = cloneValue(r.Value).(map[string]any)
	}
	return r
}

// completeGraphFor builds a fully configured DeepSeek graph whose models carry
// the given reasoning efforts and whose route points at selectedModel.
func completeGraphFor(selectedModel, proEffort, flashEffort string) configgraph.Graph {
	return configgraph.Graph{
		Revision: "r7",
		Resources: []configgraph.Resource{
			{Kind: configgraph.ResourceProvider, ID: ProviderID, Value: providerValue(apiKeyMasked)},
			{Kind: configgraph.ResourceModel, ID: ModelPro, Value: modelValue(ModelPro, proEffort)},
			{Kind: configgraph.ResourceModel, ID: ModelFlash, Value: modelValue(ModelFlash, flashEffort)},
			{Kind: configgraph.ResourceProviderOffer, ID: ProviderID + "/" + ModelPro, Value: offerValue(ModelPro, offerProPrices)},
			{Kind: configgraph.ResourceProviderOffer, ID: ProviderID + "/" + ModelFlash, Value: offerValue(ModelFlash, offerFlashPrices)},
			{Kind: configgraph.ResourceRoute, ID: RouteID, Value: routeValue(selectedModel)},
		},
	}
}

func graphWithProviderAPIKey(key string) configgraph.Graph {
	return configgraph.Graph{
		Revision: "r1",
		Resources: []configgraph.Resource{
			{Kind: configgraph.ResourceProvider, ID: ProviderID, Value: map[string]any{"api_key": key, "base_url": BaseURL}},
		},
	}
}

func TestValidateRejectsUnsupportedDefaultModel(t *testing.T) {
	err := (Input{DefaultModel: "gpt", ProReasoning: "high", FlashReasoning: "high"}).Validate()
	var se *ServiceError
	if !errors.As(err, &se) || se.Kind != ServiceErrorKindInvalidInput || se.Field == nil || *se.Field != "defaultModel" {
		t.Fatalf("expected defaultModel validation error, got %v", err)
	}
}

func TestValidateRejectsUnsupportedReasoning(t *testing.T) {
	if err := (Input{DefaultModel: "pro", ProReasoning: "low", FlashReasoning: "high"}).Validate(); err != nil {
		t.Fatalf("expected legacy Pro low to migrate successfully, got %v", err)
	}
	if err := (Input{DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "bogus"}).Validate(); err == nil {
		t.Fatal("expected flashReasoning bogus to be rejected")
	}
	if err := (Input{DefaultModel: "flash", ProReasoning: "max", FlashReasoning: "low"}).Validate(); err != nil {
		t.Fatalf("expected valid reasoning to pass, got %v", err)
	}
}

func TestValidateAcceptsXHighAsMax(t *testing.T) {
	in := Input{DefaultModel: "pro", ProReasoning: "xhigh", FlashReasoning: "xhigh"}
	if err := in.Validate(); err != nil {
		t.Fatalf("expected xhigh to normalize to max and validate, got %v", err)
	}
	norm := in.Normalized()
	if norm.ProReasoning != ReasoningMax || norm.FlashReasoning != ReasoningMax {
		t.Fatalf("expected xhigh to normalize to max, got %q and %q", norm.ProReasoning, norm.FlashReasoning)
	}
}

func TestNormalizeModelReasoningEffortMigratesLegacyProValues(t *testing.T) {
	for _, legacy := range []string{ReasoningLow, "medium"} {
		if got := NormalizeModelReasoningEffort(ModelPro, legacy); got != ReasoningHigh {
			t.Fatalf("expected Pro %q to migrate to high, got %q", legacy, got)
		}
	}
	if got := NormalizeModelReasoningEffort(ModelFlash, "medium"); got != "medium" {
		t.Fatalf("expected Flash medium to remain invalid for validation, got %q", got)
	}
}

func TestValidateRejectsLegacyFlashMedium(t *testing.T) {
	if err := (Input{DefaultModel: "flash", ProReasoning: ReasoningHigh, FlashReasoning: "medium"}).Validate(); err == nil {
		t.Fatal("expected Flash medium to be rejected")
	}
}

func TestValidateAPIKey(t *testing.T) {
	base := Input{DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "high"}
	if err := base.Validate(); err != nil {
		t.Fatalf("empty api key should be valid (keep existing), got %v", err)
	}
	if err := (Input{APIKey: "sk-abc", DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "high"}).Validate(); err == nil {
		t.Fatal("expected short api key to be rejected")
	}
	if err := (Input{APIKey: "nokeyvalue123456", DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "high"}).Validate(); err == nil {
		t.Fatal("expected non-sk api key to be rejected")
	}
	if err := (Input{APIKey: "sk-abcdefgh1234", DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "high"}).Validate(); err != nil {
		t.Fatalf("expected valid api key to pass, got %v", err)
	}
}

func TestSaveCreatesMissingResourcesAndReflectsDefaultModel(t *testing.T) {
	fake := newFakeAPI()
	svc := NewService(fake)
	input := Input{APIKey: "sk-test-key-12345", DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "max"}
	snap, err := svc.Save(context.Background(), input)
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if !snap.Configured || !snap.Active {
		t.Fatalf("expected configured+active snapshot, got %+v", snap)
	}
	if snap.SelectedModel != ModelPro || snap.DefaultModel != "pro" {
		t.Fatalf("expected pro selection, got %q/%q", snap.SelectedModel, snap.DefaultModel)
	}
	if snap.ReasoningEffort != ReasoningHigh {
		t.Fatalf("expected reasoning effort high, got %q", snap.ReasoningEffort)
	}
	if snap.Pro.Reasoning != ReasoningHigh || snap.Flash.Reasoning != ReasoningMax {
		t.Fatalf("expected per-model reasoning high/max, got %q/%q", snap.Pro.Reasoning, snap.Flash.Reasoning)
	}
	if !snap.APIKeySet || snap.APIKeyMasked != apiKeyMasked {
		t.Fatalf("expected api key set + masked, got set=%v masked=%q", snap.APIKeySet, snap.APIKeyMasked)
	}
	if fake.baseConflicts != 0 {
		t.Fatalf("expected no stale base revisions across mutations, got %d conflicts", fake.baseConflicts)
	}
	route := fake.find(configgraph.ResourceRoute, RouteID)
	if route == nil || route.Value["model"] != ModelPro || route.Value["provider"] != ProviderID || route.Value["display_name"] != "Moon Bridge" {
		t.Fatal("unexpected route value")
	}
	models := []string{ModelPro, ModelFlash}
	for _, model := range models {
		if fake.find(configgraph.ResourceModel, model) == nil {
			t.Fatalf("model %s not created", model)
		}
		if fake.find(configgraph.ResourceProviderOffer, ProviderID+"/"+model) == nil {
			t.Fatalf("offer %s not created", model)
		}
	}
	if fake.find(configgraph.ResourceProvider, ProviderID) == nil {
		t.Fatal("provider not created")
	}
}

func TestSaveReflectsFlashDefaultModel(t *testing.T) {
	fake := newFakeAPI()
	svc := NewService(fake)
	input := Input{APIKey: "sk-test-key-12345", DefaultModel: "flash", ProReasoning: "high", FlashReasoning: "low"}
	snap, err := svc.Save(context.Background(), input)
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if snap.SelectedModel != ModelFlash || snap.DefaultModel != "flash" {
		t.Fatalf("expected flash selection, got %q/%q", snap.SelectedModel, snap.DefaultModel)
	}
	route := fake.find(configgraph.ResourceRoute, RouteID)
	if route == nil || route.Value["model"] != ModelFlash {
		t.Fatal("route should point at flash")
	}
}

func TestSaveKeepsExistingAPIKeyOnEmptyInput(t *testing.T) {
	fake := newFakeAPI()
	for _, r := range completeGraphFor(ModelPro, ReasoningHigh, ReasoningHigh).Resources {
		fake.resources = append(fake.resources, cloneResource(r))
	}
	fake.revision = 5
	svc := NewService(fake)
	input := Input{DefaultModel: "pro", ProReasoning: ReasoningHigh, FlashReasoning: ReasoningHigh}
	snap, err := svc.Save(context.Background(), input)
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if fake.patches != 0 {
		t.Fatalf("expected no patches on an already-complete graph, got %d", fake.patches)
	}
	if !snap.APIKeySet {
		t.Fatal("expected existing api key to remain set")
	}
}

func TestSaveSetsNewAPIKey(t *testing.T) {
	fake := newFakeAPI()
	for _, r := range completeGraphFor(ModelPro, ReasoningHigh, ReasoningHigh).Resources {
		fake.resources = append(fake.resources, cloneResource(r))
	}
	fake.revision = 5
	svc := NewService(fake)
	input := Input{APIKey: "sk-new-key-12345", DefaultModel: "pro", ProReasoning: ReasoningHigh, FlashReasoning: ReasoningHigh}
	snap, err := svc.Save(context.Background(), input)
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}
	provider := fake.find(configgraph.ResourceProvider, ProviderID)
	if provider == nil || valueString(provider.Value, "api_key") != "sk-new-key-12345" {
		t.Fatal("expected api key to be patched to the input value")
	}
	if !snap.APIKeySet || snap.APIKeyMasked != apiKeyMasked {
		t.Fatalf("expected snapshot to keep the key masked, got %q", snap.APIKeyMasked)
	}
}

func TestSaveReportsMutationStartedOnPartialFailure(t *testing.T) {
	fake := newFakeAPI()
	fake.failAt = 2
	fake.mutateErr = errors.New("injected transport failure")
	svc := NewService(fake)
	input := Input{APIKey: "sk-test-key-12345", DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "high"}
	_, err := svc.Save(context.Background(), input)
	var se *ServiceError
	if !errors.As(err, &se) {
		t.Fatalf("expected ServiceError, got %v", err)
	}
	if !se.MutationStarted {
		t.Fatal("expected MutationStarted=true after a committed mutation followed by failure")
	}
	if se.Kind != ServiceErrorKindGatewayAPIFailed || !se.Retryable {
		t.Fatalf("expected gateway_api_failed retryable, got kind=%s retryable=%v", se.Kind, se.Retryable)
	}
}

func TestSaveRetriesRevisionConflictAndSucceeds(t *testing.T) {
	fake := newFakeAPI()
	fake.conflictN = 1
	svc := NewService(fake)
	input := Input{APIKey: "sk-test-key-12345", DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "high"}
	snap, err := svc.Save(context.Background(), input)
	if err != nil {
		t.Fatalf("save failed after retry: %v", err)
	}
	if !snap.Configured {
		t.Fatalf("expected configured snapshot after retry, got %+v", snap)
	}
}

func TestSaveReportsResidualStateAfterRepeatedConflicts(t *testing.T) {
	fake := newFakeAPI()
	fake.conflictN = 100
	svc := NewService(fake)
	input := Input{APIKey: "sk-test-key-12345", DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "high"}
	_, err := svc.Save(context.Background(), input)
	var se *ServiceError
	if !errors.As(err, &se) {
		t.Fatalf("expected ServiceError, got %v", err)
	}
	if se.Kind != ServiceErrorKindRevisionConflictExceeded || !se.MutationStarted || !se.Retryable {
		t.Fatalf("expected revision conflict exceeded with mutation started + retryable, got kind=%s", se.Kind)
	}
	if se.Details == nil || se.Details["provider"] != "missing" || se.Details["route"] != "missing" {
		t.Fatalf("expected residual partial state in details, got %v", se.Details)
	}
}

func TestSaveVerifyFailureReportsMismatch(t *testing.T) {
	fake := newFakeAPI()
	fake.corruptReasoning = true
	for _, r := range completeGraphFor(ModelPro, ReasoningLow, ReasoningLow).Resources {
		fake.resources = append(fake.resources, cloneResource(r))
	}
	fake.revision = 5
	svc := NewService(fake)
	input := Input{DefaultModel: "pro", ProReasoning: ReasoningHigh, FlashReasoning: ReasoningHigh}
	_, err := svc.Save(context.Background(), input)
	var se *ServiceError
	if !errors.As(err, &se) || se.Kind != ServiceErrorKindVerifyFailed {
		t.Fatalf("expected verify failure, got %v", err)
	}
	if !se.MutationStarted {
		t.Fatal("expected MutationStarted=true on verify failure after mutations")
	}
	if se.Details == nil || se.Details["final_state_mismatch"] != "model "+ModelPro+" reasoning" {
		t.Fatalf("expected reasoning mismatch detail, got %v", se.Details)
	}
}

func TestSaveIsIdempotentOnSecondRun(t *testing.T) {
	fake := newFakeAPI()
	svc := NewService(fake)
	input := Input{APIKey: "sk-test-key-12345", DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "max"}
	if _, err := svc.Save(context.Background(), input); err != nil {
		t.Fatalf("first save failed: %v", err)
	}
	createsBefore, patchesBefore := fake.creates, fake.patches
	second := Input{DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "max"}
	snap, err := svc.Save(context.Background(), second)
	if err != nil {
		t.Fatalf("second save failed: %v", err)
	}
	if fake.creates != createsBefore || fake.patches != patchesBefore {
		t.Fatalf("expected no mutations on second run, creates %d->%d patches %d->%d", createsBefore, fake.creates, patchesBefore, fake.patches)
	}
	if !snap.Configured {
		t.Fatalf("expected configured snapshot, got %+v", snap)
	}
}

func TestSaveRequiresAPIKeyOnFirstSetup(t *testing.T) {
	fake := newFakeAPI()
	svc := NewService(fake)
	input := Input{DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "high"}
	_, err := svc.Save(context.Background(), input)
	var se *ServiceError
	if !errors.As(err, &se) || se.Kind != ServiceErrorKindAPIKeyRequired {
		t.Fatalf("expected api_key_required, got %v", err)
	}
}

func TestLoadReturnsSnapshot(t *testing.T) {
	fake := newFakeAPI()
	for _, r := range completeGraphFor(ModelPro, ReasoningHigh, ReasoningMax).Resources {
		fake.resources = append(fake.resources, cloneResource(r))
	}
	fake.revision = 5
	svc := NewService(fake)
	snap, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if !snap.GatewayRunning || !snap.Configured || !snap.Active {
		t.Fatalf("expected running+configured+active snapshot, got %+v", snap)
	}
}

func TestClearRemovesStoredAPIKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	fake := newFakeAPI()
	fake.resources = append(fake.resources, cloneResource(configgraph.Resource{
		Kind: configgraph.ResourceProvider, ID: ProviderID,
		Value: map[string]any{"api_key": "sk-raw-12345", "base_url": BaseURL},
	}))
	fake.revision = 1
	svc := NewService(fake)

	snap, err := svc.Clear(context.Background())
	if err != nil {
		t.Fatalf("clear failed: %v", err)
	}
	if fake.mutations != 1 {
		t.Fatalf("mutations = %d, want 1", fake.mutations)
	}
	provider := fake.find(configgraph.ResourceProvider, ProviderID)
	if provider == nil {
		t.Fatal("provider missing after clear")
	}
	if k := valueString(provider.Value, "api_key"); k != "" {
		t.Fatalf("provider api_key = %q, want cleared", k)
	}
	// The provider stays present but keyless; the snapshot reports no key.
	if !snap.ProviderExists {
		t.Fatal("ProviderExists = false, want the provider to remain present")
	}
	if snap.APIKeySet || snap.APIKeyMasked != "" {
		t.Fatalf("snapshot api key = set:%v masked:%q, want cleared", snap.APIKeySet, snap.APIKeyMasked)
	}
	if snap.CredentialSource != string(CredentialSourceNone) || snap.CredentialState != string(CredentialStateMissing) {
		t.Fatalf("credential = %q/%q, want none/missing after clear", snap.CredentialSource, snap.CredentialState)
	}
}

func TestClearIsIdempotentWhenProviderMissing(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	fake := newFakeAPI()
	svc := NewService(fake)

	snap, err := svc.Clear(context.Background())
	if err != nil {
		t.Fatalf("clear failed: %v", err)
	}
	if fake.mutations != 0 {
		t.Fatalf("mutations = %d, want 0 (no provider to clear)", fake.mutations)
	}
	if snap.ProviderExists {
		t.Fatal("ProviderExists = true, want false")
	}
}

func TestClearIsIdempotentWhenProviderHasNoKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	fake := newFakeAPI()
	fake.resources = append(fake.resources, cloneResource(configgraph.Resource{
		Kind: configgraph.ResourceProvider, ID: ProviderID,
		Value: map[string]any{"api_key": "", "base_url": BaseURL},
	}))
	fake.revision = 1
	svc := NewService(fake)

	snap, err := svc.Clear(context.Background())
	if err != nil {
		t.Fatalf("clear failed: %v", err)
	}
	if fake.mutations != 0 {
		t.Fatalf("mutations = %d, want 0 (already keyless)", fake.mutations)
	}
	if snap.APIKeySet {
		t.Fatal("APIKeySet = true, want false")
	}
}

func TestClearRetriesRevisionConflictAndSucceeds(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	fake := newFakeAPI()
	fake.resources = append(fake.resources, cloneResource(configgraph.Resource{
		Kind: configgraph.ResourceProvider, ID: ProviderID,
		Value: map[string]any{"api_key": "sk-raw-12345", "base_url": BaseURL},
	}))
	fake.revision = 1
	fake.conflictN = 1
	svc := NewService(fake)

	snap, err := svc.Clear(context.Background())
	if err != nil {
		t.Fatalf("clear failed after retry: %v", err)
	}
	if fake.mutations != 2 {
		t.Fatalf("mutations = %d, want 2 (one conflicted + one committed)", fake.mutations)
	}
	if snap.APIKeySet {
		t.Fatal("APIKeySet = true, want false after retried clear")
	}
}

func TestSnapshotReMasksAPIKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "")
	snap := SnapshotFromGraph(graphWithProviderAPIKey(apiKeyMasked), true)
	if !snap.APIKeySet || snap.APIKeyMasked != apiKeyMasked {
		t.Fatalf("expected masked form preserved, got set=%v masked=%q", snap.APIKeySet, snap.APIKeyMasked)
	}
	snapPlain := SnapshotFromGraph(graphWithProviderAPIKey("sk-plaintext-key-12345"), true)
	if !snapPlain.APIKeySet || snapPlain.APIKeyMasked != "configured" {
		t.Fatalf("expected plaintext re-masked to configured, got %q", snapPlain.APIKeyMasked)
	}
	snapEmpty := SnapshotFromGraph(graphWithProviderAPIKey(""), true)
	if snapEmpty.APIKeySet || snapEmpty.APIKeyMasked != "" {
		t.Fatalf("expected unset key, got set=%v masked=%q", snapEmpty.APIKeySet, snapEmpty.APIKeyMasked)
	}
}

func TestDeriveCredential(t *testing.T) {
	env := map[string]string{"MY_KEY": "sk-set"}
	lookup := func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
	cases := []struct {
		name           string
		apiKeyRaw      string
		apiKeyEnv      string
		gatewayRunning bool
		wantSource     CredentialSource
		wantState      CredentialState
	}{
		{"stored running", "sk-stored", "DEEPSEEK_API_KEY", true, CredentialSourceStored, CredentialStateAvailable},
		{"stored stopped", "sk-stored", "DEEPSEEK_API_KEY", false, CredentialSourceStored, CredentialStateUnverified},
		{"env running", "", "MY_KEY", true, CredentialSourceEnvironment, CredentialStateAvailable},
		{"env stopped", "", "MY_KEY", false, CredentialSourceEnvironment, CredentialStateAvailable},
		{"env named but unset", "", "MISSING", true, CredentialSourceNone, CredentialStateMissing},
		{"nothing", "", "", true, CredentialSourceNone, CredentialStateMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, state := DeriveCredential(tc.apiKeyRaw, tc.apiKeyEnv, tc.gatewayRunning, lookup)
			if source != tc.wantSource || state != tc.wantState {
				t.Fatalf("got source=%q state=%q, want source=%q state=%q", source, state, tc.wantSource, tc.wantState)
			}
		})
	}
}

func TestSnapshotCredentialState(t *testing.T) {
	t.Run("stored running is available", func(t *testing.T) {
		snap := SnapshotFromGraph(graphWithProviderAPIKey("sk-raw-12345"), true)
		if snap.CredentialSource != string(CredentialSourceStored) || snap.CredentialState != string(CredentialStateAvailable) {
			t.Fatalf("got %q/%q, want stored/available", snap.CredentialSource, snap.CredentialState)
		}
	})
	t.Run("stored stopped is unverified without a probe", func(t *testing.T) {
		snap := SnapshotFromGraph(graphWithProviderAPIKey("sk-raw-12345"), false)
		if snap.CredentialSource != string(CredentialSourceStored) || snap.CredentialState != string(CredentialStateUnverified) {
			t.Fatalf("got %q/%q, want stored/unverified", snap.CredentialSource, snap.CredentialState)
		}
	})
	t.Run("env key resolves while stopped", func(t *testing.T) {
		t.Setenv("DEEPSEEK_API_KEY", "sk-env-12345")
		snap := SnapshotFromGraph(graphWithProviderAPIKey(""), false)
		if snap.CredentialSource != string(CredentialSourceEnvironment) || snap.CredentialState != string(CredentialStateAvailable) {
			t.Fatalf("got %q/%q, want environment/available", snap.CredentialSource, snap.CredentialState)
		}
		if !snap.APIKeySet {
			t.Fatal("expected env key to count as set")
		}
	})
	t.Run("nothing resolves missing", func(t *testing.T) {
		t.Setenv("DEEPSEEK_API_KEY", "")
		snap := SnapshotFromGraph(graphWithProviderAPIKey(""), true)
		if snap.CredentialSource != string(CredentialSourceNone) || snap.CredentialState != string(CredentialStateMissing) {
			t.Fatalf("got %q/%q, want none/missing", snap.CredentialSource, snap.CredentialState)
		}
	})
}

func TestSnapshotFieldsMatchCompleteGraph(t *testing.T) {
	snap := SnapshotFromGraph(completeGraphFor(ModelPro, ReasoningHigh, ReasoningMax), true)
	if !snap.GatewayRunning || !snap.ProviderExists || !snap.APIKeySet || !snap.Configured || !snap.Active {
		t.Fatalf("unexpected status fields: %+v", snap)
	}
	if snap.SelectedModel != ModelPro || snap.DefaultModel != "pro" {
		t.Fatalf("unexpected selection: %q/%q", snap.SelectedModel, snap.DefaultModel)
	}
	if snap.ReasoningEffort != ReasoningHigh || !snap.ReasoningExplicitlyConfigured {
		t.Fatalf("unexpected reasoning: %q explicit=%v", snap.ReasoningEffort, snap.ReasoningExplicitlyConfigured)
	}
	if len(snap.AllowedReasoningEfforts) != 2 || snap.AllowedReasoningEfforts[0] != ReasoningHigh || snap.AllowedReasoningEfforts[1] != ReasoningMax {
		t.Fatalf("unexpected allowed efforts: %v", snap.AllowedReasoningEfforts)
	}
	if snap.RouteAlias != RouteID {
		t.Fatalf("unexpected route alias: %q", snap.RouteAlias)
	}
	if snap.Pro.Reasoning != ReasoningHigh || snap.Flash.Reasoning != ReasoningMax || len(snap.Flash.Supported) != 3 {
		t.Fatalf("unexpected model configs: %+v", snap)
	}
}

func TestNormalizeReasoningEffort(t *testing.T) {
	if NormalizeReasoningEffort(legacyXHighReasoning) != ReasoningMax {
		t.Fatalf("expected xhigh -> max")
	}
	if NormalizeReasoningEffort(ReasoningHigh) != ReasoningHigh {
		t.Fatalf("expected high unchanged")
	}
}

func TestHTTPClientRejectsNonLoopbackBaseURL(t *testing.T) {
	c := NewHTTPClient("http://example.com:38440", "token")
	if _, err := c.Graph(context.Background()); err == nil {
		t.Fatal("expected non-loopback management API to be rejected")
	}
	if isLoopbackBaseURL("http://127.0.0.1:38440") == false {
		t.Fatal("expected loopback address to be accepted")
	}
	if isLoopbackBaseURL("http://localhost:38440") == false {
		t.Fatal("expected localhost to be accepted")
	}
}

// TestRealWireFixtureAgreement uses a graph fixture shaped exactly like the real
// gateway wire format (offer priority omitted by omitempty, masked api_key) and
// asserts SnapshotFromGraph and verifyFinalGraph agree on it: a graph that
// verifies also reports configured=true. This guards against a regression where
// the save verify passes but the snapshot reports unconfigured.
func TestRealWireFixtureAgreement(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "graph_deepseek.json"))
	if err != nil {
		t.Fatalf("read wire fixture: %v", err)
	}
	var graph configgraph.Graph
	if err := json.Unmarshal(data, &graph); err != nil {
		t.Fatalf("decode wire fixture: %v", err)
	}

	// The fixture represents a completed save (route.model=pro, reasoning high).
	input := Input{APIKey: "sk-abcdefgh", DefaultModel: "pro", ProReasoning: "high", FlashReasoning: "high"}
	if detail, ok := verifyFinalGraph(graph, input); !ok {
		t.Fatalf("verifyFinalGraph rejected a real-wire configured graph: %v", detail)
	}

	snap := SnapshotFromGraph(graph, true)
	if !snap.Configured {
		t.Fatalf("SnapshotFromGraph.Configured = false, want true (must agree with verify): %#v", snap)
	}
	if !snap.Active {
		t.Fatal("SnapshotFromGraph.Active = false, want true")
	}
	if !snap.APIKeySet {
		t.Fatal("SnapshotFromGraph.APIKeySet = false, want true (masked key still counts as set)")
	}
	if snap.APIKeyMasked == "" || snap.APIKeyMasked == "sk-abcdefgh" {
		t.Fatalf("APIKeyMasked = %q, want non-empty mask and no plaintext", snap.APIKeyMasked)
	}
	if snap.SelectedModel != ModelPro {
		t.Fatalf("SelectedModel = %q, want %q", snap.SelectedModel, ModelPro)
	}
}
