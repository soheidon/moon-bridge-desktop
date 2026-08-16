package traffictransaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/trafficanalysis"
)

type fakeIDs struct{ value string }

func (f fakeIDs) New() string { return f.value }

type sequenceIDs struct {
	mu     sync.Mutex
	values []string
	index  int
}

func (f *sequenceIDs) New() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.index >= len(f.values) {
		return ""
	}
	value := f.values[f.index]
	f.index++
	return value
}

type fakeGateway struct {
	snapshot    GatewaySnapshot
	err         error
	calls       int
	failOnCall  int
	panicOnCall int
	blockOnCall int
	entered     chan struct{}
	release     chan struct{}
	blockOnce   sync.Once
}

func (f *fakeGateway) Snapshot(context.Context) (GatewaySnapshot, error) {
	f.calls++
	if f.panicOnCall == f.calls {
		panic("sentinel gateway panic")
	}
	if f.blockOnCall == f.calls {
		f.blockOnce.Do(func() { close(f.entered) })
		<-f.release
		f.snapshot.Running = false
	}
	if f.failOnCall == f.calls {
		f.snapshot.Running = false
	}
	snapshot := f.snapshot
	if snapshot.DefaultModelAlias == "" {
		snapshot.DefaultModelAlias = "moonbridge"
	}
	return snapshot, f.err
}

type fakeConfig struct {
	mu                    sync.Mutex
	value                 string
	present               bool
	beforeHash            string
	afterHash             string
	currentHash           string
	prepareErr            error
	commitErr             error
	commitErrOnce         bool
	commitErrAfterWrite   bool
	restoreVerifyMismatch bool
	commitCalls           int
	reads                 int
	routing               codexconfig.RoutingIdentitySnapshot
	routingErr            error
	routingEmpty          bool
}

func (f *fakeConfig) ReadRoutingIdentity(context.Context) (codexconfig.RoutingIdentitySnapshot, error) {
	if f.routingErr != nil {
		return codexconfig.RoutingIdentitySnapshot{}, f.routingErr
	}
	if f.routingEmpty {
		return codexconfig.RoutingIdentitySnapshot{Model: "", ModelProvider: "openai", ConfigHash: f.currentHash}, nil
	}
	if f.routing.Model == "" {
		return codexconfig.RoutingIdentitySnapshot{Model: "gpt-test", ModelProvider: "openai", ConfigHash: f.currentHash}, nil
	}
	return f.routing, nil
}

func (f *fakeConfig) ReadRootURL(context.Context) (codexconfig.RootURLSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.present {
		if f.restoreVerifyMismatch && f.currentHash == f.beforeHash && f.commitCalls >= 2 {
			return codexconfig.RootURLSnapshot{Present: true, Value: "SENTINEL_UNEXPECTED", Hash: "url", ConfigHash: "wrong"}, nil
		}
		return codexconfig.RootURLSnapshot{Present: true, Value: f.value, Hash: "url", ConfigHash: f.currentHash}, nil
	}
	return codexconfig.RootURLSnapshot{ConfigHash: f.currentHash}, nil
}

func (f *fakeConfig) PrepareRootURLChange(_ context.Context, desired *string, expected string) (*codexconfig.PreparedRootURLChange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	if expected != "" && expected != f.currentHash {
		return nil, &codexconfig.Error{Kind: codexconfig.KindConflict, Message: "conflict"}
	}
	value := ""
	present := desired != nil
	if present {
		value = *desired
	}
	afterHash := f.beforeHash
	if present && value == captureURL {
		afterHash = f.afterHash
	}
	return &codexconfig.PreparedRootURLChange{
		BeforeHash:      expected,
		AfterHash:       afterHash,
		Present:         present,
		Value:           value,
		PreviousPresent: f.present,
		PreviousValue:   f.value,
	}, nil
}

func (f *fakeConfig) CommitPreparedRootURLChange(_ context.Context, p *codexconfig.PreparedRootURLChange) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitCalls++
	if f.commitErr != nil {
		if f.commitErrAfterWrite {
			f.present = p.Present
			f.value = p.Value
			f.currentHash = p.AfterHash
		}
		if f.commitErrOnce {
			defer func() { f.commitErr = nil }()
		}
		return f.commitErr
	}
	f.present = p.Present
	f.value = p.Value
	f.currentHash = p.AfterHash
	return nil
}

type fakeBackup struct {
	created         int
	removed         int
	err             error
	unrelatedExists bool
	unrelatedSize   int64
	unrelatedHash   string
}

func (f *fakeBackup) unrelatedFingerprint() (bool, int64, string) {
	return f.unrelatedExists, f.unrelatedSize, f.unrelatedHash
}

func (f *fakeBackup) Create(context.Context) (BackupRef, error) {
	f.created++
	if f.err != nil {
		return BackupRef{}, f.err
	}
	return BackupRef{ID: "backup-1"}, nil
}

func (f *fakeBackup) Remove(context.Context, BackupRef) error {
	f.removed++
	return nil
}

type fakeFrontDoor struct {
	mu    sync.Mutex
	last  string
	calls int
	err   error
}

func (f *fakeFrontDoor) switchUpstream(base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.last = base
	return nil
}

type fakeRecovery struct {
	cleanup             *CleanupPending
	cleanupErr          error
	clearErr            error
	mu                  sync.Mutex
	unresolved          bool
	failPhase           Phase
	failOnCall          int
	callCount           int
	checkpoints         []Checkpoint
	block               bool
	entered             chan struct{}
	release             chan struct{}
	once                sync.Once
	onCheckpointFailure func()
	onCheckpointSuccess func(Checkpoint)
	currentOverride     *Checkpoint
}

func (f *fakeRecovery) HasUnresolved(context.Context) (bool, error) {
	if f.block {
		f.once.Do(func() { close(f.entered) })
		<-f.release
	}
	return f.unresolved, nil
}

func (f *fakeRecovery) Checkpoint(_ context.Context, cp Checkpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	if cp.Phase == f.failPhase || (f.failOnCall != 0 && f.callCount == f.failOnCall) {
		if f.onCheckpointFailure != nil {
			f.onCheckpointFailure()
		}
		return errors.New("checkpoint failure")
	}
	f.checkpoints = append(f.checkpoints, cp)
	if f.onCheckpointSuccess != nil {
		f.onCheckpointSuccess(cp)
	}
	return nil
}

func (f *fakeRecovery) SetCleanupPending(_ context.Context, pending CleanupPending) error {
	if f.cleanupErr != nil {
		return f.cleanupErr
	}
	f.cleanup = &pending
	return nil
}

func (f *fakeRecovery) GetCleanupPending(context.Context) (*CleanupPending, error) {
	if f.cleanupErr != nil {
		return nil, f.cleanupErr
	}
	return f.cleanup, nil
}

func (f *fakeRecovery) ClearCleanupPending(_ context.Context, transactionID, backupID string) error {
	if f.clearErr != nil {
		return f.clearErr
	}
	if f.cleanup != nil && f.cleanup.TransactionID == transactionID && f.cleanup.BackupID == backupID {
		f.cleanup = nil
	}
	return nil
}

func (f *fakeRecovery) Current(context.Context) (Checkpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.checkpoints) == 0 {
		return Checkpoint{}, errors.New("no checkpoint")
	}
	if f.currentOverride != nil {
		return *f.currentOverride, nil
	}
	return f.checkpoints[len(f.checkpoints)-1], nil
}

type fakeTraffic struct {
	mu                sync.Mutex
	state             trafficanalysis.State
	bindCalls         int
	lastBindID        string
	lastBindAddr      string
	startCalls        int
	claimCalls        int
	releaseCalls      int
	closeCalls        int
	pauseCalls        int
	claimErr          error
	mappingSetErr     error
	mappingClearErr   error
	pauseErr          error
	startErr          error
	releaseErr        error
	closeErr          error
	blockPause        bool
	pauseEntered      chan struct{}
	pauseRelease      chan struct{}
	pauseOnce         sync.Once
	onPauseComplete   func()
	onReleaseComplete func()
	lastStart         trafficanalysis.StartOptions
	lastOwner         string
	lastReleaseOwner  string
	mappingSetCalls   int
	mappingClearCalls int
	mappingPresent    bool
	mappingSource     string
	mappingTarget     string
	mappingOwner      string
}

func (f *fakeTraffic) Status() trafficanalysis.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeTraffic) ValidateCaptureExpected(gen uint64, id, addr string) (trafficanalysis.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Operation != trafficanalysis.OperationNone || f.state.Mode != trafficanalysis.ModeCaptureOnly ||
		f.state.CaptureState != "capturing" || f.state.Generation != gen || f.state.GatewayInstanceID != id ||
		f.state.GatewayAddress != addr || f.state.ListeningAddress != CaptureListenAddress {
		return f.state, errors.New("capture is not adoptable")
	}
	return f.state, nil
}

func (f *fakeTraffic) ValidateDesktopOwnershipExpected(gen uint64, id, addr, owner string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Operation == trafficanalysis.OperationNone && f.state.Mode == trafficanalysis.ModeDesktop &&
		f.state.CaptureState == "capturing" && f.state.Generation == gen && f.state.GatewayInstanceID == id &&
		f.state.GatewayAddress == addr && f.state.ListeningAddress == CaptureListenAddress && f.lastOwner == owner
}

func (f *fakeTraffic) ValidateDesktopIntegrationExpected(gen uint64, id, addr, owner, listener string) (trafficanalysis.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Operation != trafficanalysis.OperationNone || f.state.Mode != trafficanalysis.ModeDesktop || f.state.CaptureState != "capturing" ||
		f.state.Generation != gen || f.state.GatewayInstanceID != id || f.state.GatewayAddress != addr || f.state.ListeningAddress != listener || f.lastOwner != owner {
		return f.state, errors.New("desktop integration evidence changed")
	}
	return f.state, nil
}

func (f *fakeTraffic) ValidateDesktopPassthroughExpected(gen uint64, id, addr, owner, listener string) (trafficanalysis.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Operation != trafficanalysis.OperationNone || f.state.Mode != trafficanalysis.ModeDesktop || f.state.CaptureState != "passthrough" ||
		f.state.Generation != gen || f.state.GatewayInstanceID != id || f.state.GatewayAddress != addr || f.state.ListeningAddress != listener || f.lastOwner != owner {
		return f.state, errors.New("desktop passthrough evidence changed")
	}
	return f.state, nil
}

func (f *fakeTraffic) ValidateCaptureOnlyExpected(gen uint64, id, addr, listener string) (trafficanalysis.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Operation != trafficanalysis.OperationNone || f.state.Mode != trafficanalysis.ModeCaptureOnly || f.state.CaptureState != "passthrough" ||
		f.state.Generation != gen || f.state.GatewayInstanceID != id || f.state.GatewayAddress != addr || f.state.ListeningAddress != listener {
		return f.state, errors.New("capture-only evidence changed")
	}
	return f.state, nil
}

func (f *fakeTraffic) ValidateIdleExpected(expectedGeneration uint64) (trafficanalysis.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Mode != trafficanalysis.ModeIdle || f.state.CaptureState != "stopped" || f.state.Generation <= expectedGeneration || f.state.ListeningAddress != "" || f.state.GatewayInstanceID != "" || f.state.GatewayAddress != "" {
		return f.state, errors.New("capture is not idle")
	}
	return f.state, nil
}

func (f *fakeTraffic) BindGatewayRun(id, addr string) (trafficanalysis.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindCalls++
	f.lastBindID = id
	f.lastBindAddr = addr
	f.state.GatewayInstanceID = id
	f.state.GatewayAddress = addr
	return f.state, nil
}

func (f *fakeTraffic) StartCapture(opts trafficanalysis.StartOptions) (trafficanalysis.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	f.lastStart = opts
	if f.startErr != nil {
		return f.state, f.startErr
	}
	f.state.Mode = trafficanalysis.ModeCaptureOnly
	f.state.CaptureState = "capturing"
	f.state.Generation = 1
	f.state.GatewayInstanceID = "gw-1"
	f.state.GatewayAddress = "127.0.0.1:38442"
	f.state.ListeningAddress = CaptureListenAddress
	return f.state, nil
}

func (f *fakeTraffic) ClaimDesktopExpected(gen uint64, id, addr, owner string) (trafficanalysis.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCalls++
	f.lastOwner = owner
	if f.claimErr != nil {
		return f.state, f.claimErr
	}
	if f.state.Generation != gen || f.state.GatewayInstanceID != id || f.state.GatewayAddress != addr {
		return f.state, errors.New("identity mismatch")
	}
	f.state.Mode = trafficanalysis.ModeDesktop
	return f.state, nil
}

func (f *fakeTraffic) SetDesktopModelMappingExpected(gen uint64, id, addr, owner, source, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mappingSetCalls++
	if f.mappingSetErr != nil {
		return f.mappingSetErr
	}
	if f.state.Mode != trafficanalysis.ModeDesktop || f.state.Generation != gen || f.state.GatewayInstanceID != id || f.state.GatewayAddress != addr || f.lastOwner != owner {
		return errors.New("mapping identity mismatch")
	}
	f.mappingPresent = true
	f.mappingSource = source
	f.mappingTarget = target
	f.mappingOwner = owner
	return nil
}

func (f *fakeTraffic) ClearDesktopModelMappingExpected(gen uint64, id, addr, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mappingClearCalls++
	if f.mappingClearErr != nil {
		return f.mappingClearErr
	}
	if !f.mappingPresent {
		return nil
	}
	if f.state.Generation != gen || f.state.GatewayInstanceID != id || f.state.GatewayAddress != addr || f.mappingOwner != owner {
		return errors.New("mapping clear identity mismatch")
	}
	f.mappingPresent = false
	f.mappingSource = ""
	f.mappingTarget = ""
	f.mappingOwner = ""
	return nil
}

func (f *fakeTraffic) PauseDesktopExpected(_ context.Context, _ uint64, _, _, _ string) (trafficanalysis.State, error) {
	f.mu.Lock()
	f.pauseCalls++
	if f.blockPause {
		f.pauseOnce.Do(func() { close(f.pauseEntered) })
		f.mu.Unlock()
		<-f.pauseRelease
		f.mu.Lock()
	}
	if f.pauseErr != nil {
		f.mu.Unlock()
		return f.state, f.pauseErr
	}
	f.state.CaptureState = "passthrough"
	state := f.state
	callback := f.onPauseComplete
	f.mu.Unlock()
	if callback != nil {
		callback()
	}
	return state, nil
}

func (f *fakeTraffic) ReleaseDesktopExpected(_ uint64, owner string) (trafficanalysis.State, error) {
	f.mu.Lock()
	f.releaseCalls++
	f.lastReleaseOwner = owner
	if f.releaseErr != nil {
		f.mu.Unlock()
		return f.state, f.releaseErr
	}
	f.state.Mode = trafficanalysis.ModeCaptureOnly
	f.lastOwner = ""
	state := f.state
	callback := f.onReleaseComplete
	f.mu.Unlock()
	if callback != nil {
		callback()
	}
	return state, nil
}

func (f *fakeTraffic) CloseCapture(context.Context) (trafficanalysis.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	if f.closeErr != nil {
		return f.state, f.closeErr
	}
	f.state = trafficanalysis.State{Mode: trafficanalysis.ModeIdle, CaptureState: "stopped", Generation: f.state.Generation + 1}
	f.mappingPresent = false
	f.mappingSource = ""
	f.mappingTarget = ""
	f.mappingOwner = ""
	return f.state, nil
}

func newFixture() (*Service, *fakeTraffic, *fakeConfig, *fakeBackup, *fakeRecovery) {
	service, traffic, cfg, backup, recovery, _, _ := newFixtureWithGateway()
	return service, traffic, cfg, backup, recovery
}

func newFixtureWithGateway() (*Service, *fakeTraffic, *fakeConfig, *fakeBackup, *fakeRecovery, *fakeGateway, *fakeFrontDoor) {
	gw := &fakeGateway{snapshot: GatewaySnapshot{Running: true, InstanceID: "gw-1", Address: "127.0.0.1:38442", DefaultModelAlias: "moonbridge", RoutingAvailable: true}}
	traffic := &fakeTraffic{state: trafficanalysis.State{Mode: trafficanalysis.ModeIdle, CaptureState: "stopped"}}
	cfg := &fakeConfig{beforeHash: "before", afterHash: "after", currentHash: "before", present: true, value: "https://api.openai.com"}
	backup := &fakeBackup{}
	recovery := &fakeRecovery{}
	frontDoor := &fakeFrontDoor{}
	service := New(Dependencies{
		Gateway: gw, Traffic: traffic, Config: cfg, Backup: backup, Recovery: recovery, IDs: fakeIDs{value: "tx-1"},
		SetFrontDoorUpstream: frontDoor.switchUpstream,
	})
	return service, traffic, cfg, backup, recovery, gw, frontDoor
}

func newEnabledFixture(t *testing.T) (*Service, *fakeTraffic, *fakeConfig, *fakeRecovery) {
	t.Helper()
	service, traffic, cfg, _, recovery, _, _ := newFixtureWithGateway()
	service.ids = &sequenceIDs{values: []string{"enable-1", "disable-1", "finish-1", "finish-2"}}
	if _, err := service.Enable(context.Background()); err != nil {
		t.Fatalf("Enable fixture: %v", err)
	}
	return service, traffic, cfg, recovery
}

func requireKind(t *testing.T, err error, want ErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", want)
	}
	got, ok := err.(*Error)
	if !ok || got.Kind != want {
		t.Fatalf("error = %#v, want kind %s", err, want)
	}
}

func TestClassifyFailureIsPureAndDistinguishesUncertainCheckpoint(t *testing.T) {
	plan := ClassifyFailure(FailureState{Phase: PhaseOwnershipClaimed, StartedCapture: true, OwnershipClaimed: true}, CauseConfigSave)
	if !plan.ReleaseOwnership || !plan.CloseNewCapture || plan.RestoreConfig || plan.RecoveryRequired {
		t.Fatalf("claim failure plan = %#v", plan)
	}
	uncertain := ClassifyFailure(FailureState{Phase: PhaseConfigCommitted, StartedCapture: true, OwnershipClaimed: true, ConfigCommitted: true, CheckpointUncertain: true}, CauseCheckpoint)
	if !uncertain.RecoveryRequired || uncertain.ReleaseOwnership || uncertain.RestoreConfig || uncertain.CloseNewCapture {
		t.Fatalf("uncertain plan = %#v", uncertain)
	}
}


func TestEnableStartsClaimsSwitchesAndCheckpoints(t *testing.T) {
	service, traffic, _, _, recovery, _, frontDoor := newFixtureWithGateway()
	got, err := service.Enable(context.Background())
	if err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if got.Phase != PhaseCompleted || !got.IntegrationActive || got.TrafficMode != trafficanalysis.ModeDesktop {
		t.Fatalf("result = %#v", got)
	}
	if traffic.startCalls != 1 || traffic.claimCalls != 1 || traffic.lastOwner != "tx-1" {
		t.Fatalf("calls start/claim/owner = %d/%d/%q", traffic.startCalls, traffic.claimCalls, traffic.lastOwner)
	}
	if traffic.lastStart.ListenAddr != CaptureListenAddress || traffic.lastStart.UpstreamBase != "http://127.0.0.1:38442" {
		t.Fatalf("start options = %#v", traffic.lastStart)
	}
	if frontDoor.calls != 1 || frontDoor.last != captureURL {
		t.Fatalf("front door switch = calls %d last %q, want 1/%q", frontDoor.calls, frontDoor.last, captureURL)
	}
	if len(recovery.checkpoints) != 3 || recovery.checkpoints[0].Phase != PhasePrepared || recovery.checkpoints[2].IntegrationActive != true {
		t.Fatalf("checkpoints = %#v", recovery.checkpoints)
	}
}

func TestEnableRebindsGatewayBeforeStartCapture(t *testing.T) {
	service, traffic, _, _, _, gw, _ := newFixtureWithGateway()
	if _, err := service.Enable(context.Background()); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if traffic.bindCalls != 1 || traffic.startCalls != 1 {
		t.Fatalf("bind/start calls = %d/%d, want 1/1", traffic.bindCalls, traffic.startCalls)
	}
	if traffic.lastBindID != gw.snapshot.InstanceID || traffic.lastBindAddr != gw.snapshot.Address {
		t.Fatalf("bound identity = %q/%q, want %q/%q", traffic.lastBindID, traffic.lastBindAddr, gw.snapshot.InstanceID, gw.snapshot.Address)
	}
}

func TestSuccessfulEnableDisableEmitsSafeLifecycleEventsInOrder(t *testing.T) {
	service, _, _, _, _ := newFixture()
	service.ids = &sequenceIDs{values: []string{"enable-1", "disable-1"}}
	var events []Event
	service.deps.Events = func(event Event) {
		events = append(events, event)
	}

	if _, err := service.Enable(context.Background()); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if _, err := service.Disable(context.Background()); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}

	want := []EventCode{
		EventRouteApplied,
		EventAnalysisStarted,
		EventAnalysisStopped,
	}
	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d", len(events), len(want))
	}
	for i, event := range events {
		if event.Code != want[i] {
			t.Fatalf("event %d code = %q, want %q", i, event.Code, want[i])
		}
		if event.Timestamp == "" {
			t.Fatalf("event %d timestamp is empty", i)
		}
		if event.Severity != EventSeverityInfo && event.Severity != EventSeveritySuccess {
			t.Fatalf("event %d has unexpected severity %q", i, event.Severity)
		}
	}
}

func TestEnableRegistersExactModelMapping(t *testing.T) {
	service, traffic, _, _, _ := newFixture()
	if _, err := service.Enable(context.Background()); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	// The model mapping source comes from the gateway's default model alias in
	// the front-door model (the Codex config routing identity is no longer read),
	// so the source is registered empty (lazily bound later) and the target is
	// the gateway's alias.
	if traffic.mappingSetCalls != 1 || !traffic.mappingPresent || traffic.mappingSource != "" || traffic.mappingTarget != "moonbridge" {
		t.Fatalf("mapping registration = calls %d present %v source %q target %q", traffic.mappingSetCalls, traffic.mappingPresent, traffic.mappingSource, traffic.mappingTarget)
	}
}

func TestEnableMappingRegistrationFailureLeavesConfigUnchanged(t *testing.T) {
	service, traffic, cfg, _, _ := newFixture()
	traffic.mappingSetErr = errors.New("mapping registration failed")
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindOwnershipClaimFailed)
	if cfg.commitCalls != 0 {
		t.Fatalf("config commit calls = %d, want 0", cfg.commitCalls)
	}
	if traffic.mappingPresent || traffic.closeCalls != 1 || traffic.releaseCalls != 1 {
		t.Fatalf("failed mapping cleanup = present %v close %d release %d", traffic.mappingPresent, traffic.closeCalls, traffic.releaseCalls)
	}
}



func TestEnableAdoptsExistingCaptureWithoutStarting(t *testing.T) {
	service, traffic, _, _, _ := newFixture()
	traffic.state = trafficanalysis.State{Mode: trafficanalysis.ModeCaptureOnly, CaptureState: "capturing", Generation: 7, GatewayInstanceID: "gw-1", GatewayAddress: "127.0.0.1:38442", ListeningAddress: CaptureListenAddress, ObservationCount: 3}
	got, err := service.Enable(context.Background())
	if err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if got.Phase != PhaseCompleted || traffic.startCalls != 0 || traffic.claimCalls != 1 || traffic.state.Generation != 7 || traffic.state.ObservationCount != 3 {
		t.Fatalf("adoption result/state = %#v/%#v", got, traffic.state)
	}
}

func TestEnableClaimFailureDoesNotWriteAndBacksOutNewCapture(t *testing.T) {
	service, traffic, cfg, _, recovery := newFixture()
	traffic.claimErr = errors.New("claim failed")
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindOwnershipClaimFailed)
	if cfg.commitCalls != 0 || traffic.releaseCalls != 0 || traffic.closeCalls != 1 || traffic.state.Mode != trafficanalysis.ModeIdle {
		t.Fatalf("claim backout calls/state = %d/%d/%d/%#v", cfg.commitCalls, traffic.releaseCalls, traffic.closeCalls, traffic.state)
	}
	if len(recovery.checkpoints) != 2 || recovery.checkpoints[1].Phase != PhaseAborted {
		t.Fatalf("checkpoints = %#v", recovery.checkpoints)
	}
}


func TestEnableIntegrationCheckpointFailureRequiresRecoveryAndDoesNotGuess(t *testing.T) {
	service, traffic, _, _, recovery := newFixture()
	recovery.failPhase = PhaseConfigCommitted
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindCheckpointFailed)
	if traffic.state.Mode != trafficanalysis.ModeIdle || traffic.closeCalls != 1 || traffic.releaseCalls != 1 {
		t.Fatalf("backout state/calls = %#v/%d/%d", traffic.state, traffic.closeCalls, traffic.releaseCalls)
	}
}


func TestIntegrationCheckpointFailureWithStaleOwnerRequiresRecovery(t *testing.T) {
	service, traffic, _, _, recovery := newFixture()
	recovery.failPhase = PhaseConfigCommitted
	traffic.releaseErr = errors.New("stale owner")
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.lastReleaseOwner != "tx-1" || traffic.releaseCalls != 1 || traffic.closeCalls != 0 {
		t.Fatalf("stale-owner backout = owner %q release %d close %d", traffic.lastReleaseOwner, traffic.releaseCalls, traffic.closeCalls)
	}
}

func TestAdoptionWithPublicOperationDoesNotReachConfigOrClaim(t *testing.T) {
	service, traffic, cfg, backup, _ := newFixture()
	traffic.state = trafficanalysis.State{
		Mode:              trafficanalysis.ModeCaptureOnly,
		CaptureState:      "capturing",
		Operation:         trafficanalysis.OperationStarting,
		Generation:        7,
		GatewayInstanceID: "gw-1",
		GatewayAddress:    "127.0.0.1:38442",
		ListeningAddress:  CaptureListenAddress,
	}
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindCaptureNotActive)
	if traffic.claimCalls != 0 || traffic.startCalls != 0 || cfg.commitCalls != 0 || backup.created != 0 {
		t.Fatalf("operation-active adoption had side effects: claim=%d start=%d commit=%d backup=%d", traffic.claimCalls, traffic.startCalls, cfg.commitCalls, backup.created)
	}
}

func TestGatewayEndRunAfterPreparedBacksOutWithoutStarting(t *testing.T) {
	service, traffic, cfg, _, _, gw, _ := newFixtureWithGateway()
	err := enableWithGatewayLoss(service, gw, 2)
	requireKind(t, err, KindGatewayNotRunning)
	if traffic.startCalls != 0 || traffic.claimCalls != 0 || cfg.commitCalls != 0 || traffic.closeCalls != 0 {
		t.Fatalf("prepared gateway loss side effects: start=%d claim=%d commit=%d close=%d", traffic.startCalls, traffic.claimCalls, cfg.commitCalls, traffic.closeCalls)
	}
}

func TestGatewayEndRunAfterStartClosesOnlyNewCapture(t *testing.T) {
	service, traffic, cfg, _, _, gw, _ := newFixtureWithGateway()
	err := enableWithGatewayLoss(service, gw, 3)
	requireKind(t, err, KindGatewayNotRunning)
	if traffic.startCalls != 1 || traffic.claimCalls != 0 || cfg.commitCalls != 0 || traffic.closeCalls != 1 {
		t.Fatalf("start gateway loss side effects: start=%d claim=%d commit=%d close=%d", traffic.startCalls, traffic.claimCalls, cfg.commitCalls, traffic.closeCalls)
	}
}

func TestGatewayEndRunAfterClaimRestoresOwnershipBeforeClose(t *testing.T) {
	service, traffic, cfg, _, _, gw, _ := newFixtureWithGateway()
	err := enableWithGatewayLoss(service, gw, 4)
	requireKind(t, err, KindGatewayNotRunning)
	if traffic.startCalls != 1 || traffic.claimCalls != 1 || cfg.commitCalls != 0 || traffic.releaseCalls != 1 || traffic.closeCalls != 1 || traffic.lastReleaseOwner != "tx-1" {
		t.Fatalf("claim gateway loss side effects: start=%d claim=%d commit=%d release=%d close=%d owner=%q", traffic.startCalls, traffic.claimCalls, cfg.commitCalls, traffic.releaseCalls, traffic.closeCalls, traffic.lastReleaseOwner)
	}
}

func TestGatewayEndRunAfterFrontDoorSwitchBacksOutToGateway(t *testing.T) {
	service, traffic, _, _, _, gw, _ := newFixtureWithGateway()
	err := enableWithGatewayLoss(service, gw, 6)
	requireKind(t, err, KindGatewayNotRunning)
	if traffic.releaseCalls != 1 || traffic.closeCalls != 1 {
		t.Fatalf("front-door-switch gateway loss backout: release=%d close=%d", traffic.releaseCalls, traffic.closeCalls)
	}
}

func TestGatewayEndRunAfterIntegrationCheckpointRestoresBeforeReturning(t *testing.T) {
	service, traffic, _, _, _, gw, _ := newFixtureWithGateway()
	err := enableWithGatewayLoss(service, gw, 7)
	requireKind(t, err, KindGatewayNotRunning)
	if traffic.releaseCalls != 1 || traffic.closeCalls != 1 {
		t.Fatalf("post-checkpoint gateway loss backout: release=%d close=%d", traffic.releaseCalls, traffic.closeCalls)
	}
}

func enableWithGatewayLoss(service *Service, gateway *fakeGateway, call int) error {
	gateway.blockOnCall = call
	gateway.entered = make(chan struct{})
	gateway.release = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := service.Enable(context.Background())
		done <- err
	}()
	<-gateway.entered
	close(gateway.release)
	return <-done
}

func TestOperationSlotIsGeneralAndStaleReleaseCannotClearNewOwner(t *testing.T) {
	service, _, _, _, _ := newFixture()
	if err := service.reserveOperation("tx-disable", OperationDisable); err != nil {
		t.Fatalf("reserve disable: %v", err)
	}
	if _, err := service.Enable(context.Background()); err == nil {
		t.Fatal("Enable should be rejected while disable occupies the slot")
	} else {
		requireKind(t, err, KindTransactionInProgress)
	}
	service.releaseOperation("stale-id")
	if _, err := service.Enable(context.Background()); err == nil {
		t.Fatal("stale release cleared the active slot")
	}
	service.releaseOperation("tx-disable")
	for _, operation := range []Operation{OperationRestore, OperationDiscard} {
		if err := service.reserveOperation("future-"+string(operation), operation); err != nil {
			t.Fatalf("reserve %s: %v", operation, err)
		}
		if _, err := service.Enable(context.Background()); err == nil {
			t.Fatalf("Enable should be rejected while %s occupies the slot", operation)
		}
		service.releaseOperation("future-" + string(operation))
	}
	if _, err := service.Enable(context.Background()); err != nil {
		t.Fatalf("Enable after slot release: %v", err)
	}
}

func TestOperationSlotIsReleasedByDeferredPanicPath(t *testing.T) {
	service, _, _, _, _, gateway, _ := newFixtureWithGateway()
	gateway.panicOnCall = 1
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected gateway panic")
			}
		}()
		_, _ = service.Enable(context.Background())
	}()
	gateway.panicOnCall = 0
	if _, err := service.Enable(context.Background()); err != nil {
		t.Fatalf("operation slot was not released after panic: %v", err)
	}
}

func TestFailureClassifierMatrixCoversEnableBackoutStages(t *testing.T) {
	cases := []struct {
		name     string
		state    FailureState
		cause    FailureCause
		restore  bool
		release  bool
		close    bool
		recovery bool
		confirm  bool
		kind     ErrorKind
	}{
		{"adoption", FailureState{Phase: PhasePrepared, AdoptedCapture: true}, CauseAdoption, false, false, false, false, false, KindCaptureNotActive},
		{"gateway after start", FailureState{Phase: PhaseCaptureStarted, StartedCapture: true}, CauseGatewayLost, false, false, true, false, false, KindGatewayNotRunning},
		{"gateway after claim", FailureState{Phase: PhaseOwnershipClaimed, StartedCapture: true, OwnershipClaimed: true}, CauseGatewayLost, false, true, true, false, false, KindGatewayNotRunning},
		{"gateway after config", FailureState{Phase: PhaseConfigCommitted, StartedCapture: true, OwnershipClaimed: true, ConfigCommitted: true}, CauseGatewayLost, true, true, true, false, false, KindGatewayNotRunning},
		{"checkpoint uncertain", FailureState{Phase: PhaseConfigCommitted, CheckpointUncertain: true}, CauseCheckpoint, false, false, false, true, false, KindCheckpointUncertain},
		{"stale owner", FailureState{Phase: PhaseOwnershipClaimed, StartedCapture: true, OwnershipClaimed: true}, CauseStaleOwner, false, false, false, true, true, KindRecoveryRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyFailure(tc.state, tc.cause)
			if got.RestoreConfig != tc.restore || got.ReleaseOwnership != tc.release || got.CloseNewCapture != tc.close || got.RecoveryRequired != tc.recovery || got.ConfirmationRequired != tc.confirm || got.ErrorKind != tc.kind {
				t.Fatalf("plan = %#v", got)
			}
		})
	}
}

func TestFinalJournalMismatchBacksOutWithoutSuccess(t *testing.T) {
	service, traffic, _, _, recovery := newFixture()
	recovery.currentOverride = &Checkpoint{Phase: PhaseCaptureStarted, IntegrationActive: false, OperationID: "other"}
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.releaseCalls != 1 || traffic.closeCalls != 1 {
		t.Fatalf("final journal mismatch backout: release=%d close=%d", traffic.releaseCalls, traffic.closeCalls)
	}
}

func TestStaleOwnerDuringCheckpointBackoutDoesNotChangeReplacement(t *testing.T) {
	service, traffic, _, _, recovery := newFixture()
	recovery.failPhase = PhaseConfigCommitted
	traffic.releaseErr = errors.New("owner superseded")
	recovery.onCheckpointFailure = func() {
		traffic.mu.Lock()
		traffic.state.Mode = trafficanalysis.ModeDesktop
		traffic.lastOwner = "owner-b"
		traffic.mu.Unlock()
	}
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.lastOwner != "owner-b" || traffic.state.Mode != trafficanalysis.ModeDesktop || traffic.closeCalls != 0 {
		t.Fatalf("replacement owner changed: owner=%q mode=%q close=%d", traffic.lastOwner, traffic.state.Mode, traffic.closeCalls)
	}
}

func TestIntegrationCheckpointFailureWithCloseFailureRequiresRecovery(t *testing.T) {
	service, traffic, _, _, recovery := newFixture()
	recovery.failPhase = PhaseConfigCommitted
	traffic.closeErr = errors.New("close failed")
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.releaseCalls != 1 || traffic.closeCalls != 1 || traffic.state.Mode != trafficanalysis.ModeCaptureOnly {
		t.Fatalf("close failure recovery = release %d close %d state %#v", traffic.releaseCalls, traffic.closeCalls, traffic.state)
	}
}


func TestFinalAtomicOwnerMismatchEntersRecoveryWithoutChangingReplacement(t *testing.T) {
	service, traffic, _, _, recovery := newFixture()
	traffic.releaseErr = errors.New("owner superseded")
	recovery.onCheckpointSuccess = func(cp Checkpoint) {
		if cp.Phase == PhaseConfigCommitted {
			traffic.mu.Lock()
			traffic.lastOwner = "owner-b"
			traffic.mu.Unlock()
		}
	}
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.lastOwner != "owner-b" || traffic.releaseCalls != 1 || traffic.closeCalls != 0 {
		t.Fatalf("final atomic owner mismatch = owner %q release %d close %d", traffic.lastOwner, traffic.releaseCalls, traffic.closeCalls)
	}
}

func TestSafeResultAndErrorsDoNotExposeTransactionOrLowerLevelData(t *testing.T) {
	result := Snapshot{Operation: OperationEnable, Phase: PhaseCompleted, TrafficMode: trafficanalysis.ModeDesktop}
	err := safeError(KindConfigSaveFailed, "config could not be written", true)
	resultJSON, _ := json.Marshal(result)
	errJSON, _ := json.Marshal(err)
	for _, data := range []string{string(resultJSON), string(errJSON)} {
		for _, secret := range []string{"tx-1", "SENTINEL_TOKEN", "secret-url", "private\\config"} {
			if strings.Contains(data, secret) {
				t.Fatalf("safe surface contains %q: %s", secret, data)
			}
		}
	}
}

func TestEnableRejectsUnresolvedRecoveryBeforeSideEffects(t *testing.T) {
	service, traffic, cfg, backup, recovery := newFixture()
	recovery.unresolved = true
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.startCalls != 0 || cfg.commitCalls != 0 || backup.created != 0 {
		t.Fatalf("side effects occurred: start=%d commit=%d backup=%d", traffic.startCalls, cfg.commitCalls, backup.created)
	}
}

func TestEnableIsSingleFlight(t *testing.T) {
	service, _, _, _, recovery := newFixture()
	recovery.block = true
	recovery.entered = make(chan struct{})
	recovery.release = make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Enable(context.Background())
		firstDone <- err
	}()
	<-recovery.entered
	_, err := service.Enable(context.Background())
	requireKind(t, err, KindTransactionInProgress)
	close(recovery.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Enable() error = %v", err)
	}
}

func TestDisableSuccessPausesReleasesAndDemotesWithoutClosing(t *testing.T) {
	service, traffic, _, recovery := newEnabledFixture(t)
	beforeGeneration := traffic.state.Generation
	_, err := service.Disable(context.Background())
	if err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if traffic.pauseCalls != 1 || traffic.releaseCalls != 1 || traffic.closeCalls != 0 || !traffic.mappingPresent || traffic.lastReleaseOwner != "enable-1" || traffic.lastOwner != "" || traffic.state.Mode != trafficanalysis.ModeCaptureOnly || traffic.state.CaptureState != "passthrough" || traffic.state.Generation != beforeGeneration {
		t.Fatalf("disable traffic state/calls = %#v pause=%d release=%d close=%d", traffic.state, traffic.pauseCalls, traffic.releaseCalls, traffic.closeCalls)
	}
	if len(recovery.checkpoints) == 0 || recovery.checkpoints[len(recovery.checkpoints)-1].DurablePhase != DurableInactive || recovery.checkpoints[len(recovery.checkpoints)-1].OperationID != "disable-1" || recovery.checkpoints[len(recovery.checkpoints)-1].IntegrationTarget != TargetGateway {
		t.Fatalf("disable recovery demote = %#v", recovery.checkpoints)
	}
}

func TestDisablePreviousKeyAbsentDeletesOnlyManagedKey(t *testing.T) {
	service, traffic, cfg, _, recovery := newFixture()
	service.ids = &sequenceIDs{values: []string{"enable-1", "disable-1"}}
	cfg.present = false
	if _, err := service.Enable(context.Background()); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if _, err := service.Disable(context.Background()); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if cfg.present || traffic.state.Mode != trafficanalysis.ModeCaptureOnly || traffic.closeCalls != 0 || recovery.checkpoints[len(recovery.checkpoints)-1].DurablePhase != DurableInactive {
		t.Fatalf("absent-key disable state = present %v mode %q close %d", cfg.present, traffic.state.Mode, traffic.closeCalls)
	}
}



func TestDisablePauseFailureDoesNotRelease(t *testing.T) {
	service, traffic, _, _ := newEnabledFixture(t)
	traffic.pauseErr = errors.New("pause failed")
	_, err := service.Disable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.pauseCalls != 1 || traffic.releaseCalls != 0 || traffic.closeCalls != 0 || traffic.state.Mode != trafficanalysis.ModeDesktop {
		t.Fatalf("pause failure side effects = pause %d release %d close %d mode %q", traffic.pauseCalls, traffic.releaseCalls, traffic.closeCalls, traffic.state.Mode)
	}
}



func TestDisableRestoreCheckpointFailureDoesNotRelease(t *testing.T) {
	service, traffic, _, recovery := newEnabledFixture(t)
	recovery.failPhase = PhaseConfigRestored
	_, err := service.Disable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.releaseCalls != 0 || traffic.state.Mode != trafficanalysis.ModeDesktop {
		t.Fatalf("restore checkpoint failure released ownership: release %d mode %q", traffic.releaseCalls, traffic.state.Mode)
	}
}

func TestDisableReleaseFailureKeepsConfigRestoredAndRequiresRecovery(t *testing.T) {
	service, traffic, cfg, _ := newEnabledFixture(t)
	traffic.releaseErr = errors.New("release failed")
	_, err := service.Disable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.releaseCalls != 1 || traffic.closeCalls != 0 || traffic.state.Mode != trafficanalysis.ModeDesktop || cfg.currentHash != cfg.beforeHash {
		t.Fatalf("release failure state = release %d close %d mode %q hash %q", traffic.releaseCalls, traffic.closeCalls, traffic.state.Mode, cfg.currentHash)
	}
}

func TestDisableRejectsAfterProcessOwnerIsUnavailable(t *testing.T) {
	service, traffic, _, _ := newEnabledFixture(t)
	service.mu.Lock()
	service.ownerID = ""
	service.mu.Unlock()
	_, err := service.Disable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.pauseCalls != 0 || traffic.releaseCalls != 0 {
		t.Fatalf("ownerless disable performed traffic calls: pause %d release %d", traffic.pauseCalls, traffic.releaseCalls)
	}
}

func TestDisableOperationSlotConflictsWithFutureOperations(t *testing.T) {
	service, _, _, _, _, _, _ := newFixtureWithGateway()
	if err := service.reserveOperation("restore-1", OperationRestore); err != nil {
		t.Fatalf("reserve restore: %v", err)
	}
	if _, err := service.Disable(context.Background()); err == nil {
		t.Fatal("Disable should be rejected while restore owns the slot")
	} else {
		requireKind(t, err, KindTransactionInProgress)
	}
	service.releaseOperation("restore-1")
}

func TestDisableSwitchesFrontDoorBackToGatewayBeforePause(t *testing.T) {
	service, traffic, _, _, _, _, frontDoor := newFixtureWithGateway()
	service.ids = &sequenceIDs{values: []string{"enable-1", "disable-1", "finish-1", "finish-2"}}
	if _, err := service.Enable(context.Background()); err != nil {
		t.Fatalf("Enable fixture: %v", err)
	}
	traffic.pauseEntered = make(chan struct{})
	traffic.pauseRelease = make(chan struct{})
	traffic.blockPause = true

	done := make(chan error, 1)
	go func() {
		_, err := service.Disable(context.Background())
		done <- err
	}()
	select {
	case <-traffic.pauseEntered:
	case <-time.After(time.Second):
		t.Fatal("Disable did not reach PauseDesktopExpected")
	}
	// The front door must point back at the gateway backend before the capture is
	// paused, so Codex never points at a paused capture relay.
	if frontDoor.last != gatewayBackendURL {
		t.Fatalf("front door = %q, want %q before pause", frontDoor.last, gatewayBackendURL)
	}
	close(traffic.pauseRelease)
	if err := <-done; err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
}

func TestDisableRejectsGatewayChangeAfterPause(t *testing.T) {
	service, traffic, _, _ := newEnabledFixture(t)
	traffic.onPauseComplete = func() {
		traffic.mu.Lock()
		traffic.state.Mode = trafficanalysis.ModeCaptureOnly
		traffic.mu.Unlock()
	}
	_, err := service.Disable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.releaseCalls != 0 || traffic.state.Mode != trafficanalysis.ModeCaptureOnly {
		t.Fatalf("pause boundary was not revalidated: release=%d mode=%q", traffic.releaseCalls, traffic.state.Mode)
	}
}

func TestDisableRevalidatesAfterSwitchBeforeRelease(t *testing.T) {
	service, traffic, _, recovery := newEnabledFixture(t)
	recovery.onCheckpointSuccess = func(cp Checkpoint) {
		if cp.Phase == PhaseConfigRestored {
			traffic.mu.Lock()
			traffic.state.Mode = trafficanalysis.ModeCaptureOnly
			traffic.mu.Unlock()
		}
	}
	_, err := service.Disable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.releaseCalls != 0 || traffic.state.Mode != trafficanalysis.ModeCaptureOnly {
		t.Fatalf("switch boundary was not revalidated: release=%d mode=%q", traffic.releaseCalls, traffic.state.Mode)
	}
}

func TestDisableRejectsGatewayChangeAfterReleaseBeforeInactive(t *testing.T) {
	service, traffic, _, recovery := newEnabledFixture(t)
	traffic.onReleaseComplete = func() {
		traffic.mu.Lock()
		traffic.state.CaptureState = "stopped"
		traffic.state.Mode = trafficanalysis.ModeRecovery
		traffic.mu.Unlock()
	}
	_, err := service.Disable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.releaseCalls != 1 || len(recovery.checkpoints) == 0 || recovery.checkpoints[len(recovery.checkpoints)-1].DurablePhase != DurableReconciliationRequired {
		t.Fatalf("release boundary was not classified as recovery: release=%d journal=%#v", traffic.releaseCalls, recovery.checkpoints[len(recovery.checkpoints)-1])
	}
}

func TestDisableInactiveCheckpointFailureDoesNotReapplyOrReclaim(t *testing.T) {
	service, traffic, _, recovery := newEnabledFixture(t)
	recovery.failPhase = PhaseDisableCompleted
	_, err := service.Disable(context.Background())
	requireKind(t, err, KindRecoveryRequired)
	if traffic.claimCalls != 1 || traffic.releaseCalls != 1 || traffic.closeCalls != 0 || traffic.state.Mode != trafficanalysis.ModeCaptureOnly || traffic.lastOwner != "" {
		t.Fatalf("inactive checkpoint failure changed state: claims=%d releases=%d closes=%d mode=%q owner=%q", traffic.claimCalls, traffic.releaseCalls, traffic.closeCalls, traffic.state.Mode, traffic.lastOwner)
	}
	if recovery.checkpoints[len(recovery.checkpoints)-1].DurablePhase != DurableReconciliationRequired {
		t.Fatalf("inactive checkpoint failure did not retain recovery evidence: %#v", recovery.checkpoints[len(recovery.checkpoints)-1])
	}
}

func TestDisableAcceptsAlreadyPausedDesktopCapture(t *testing.T) {
	service, traffic, _, recovery := newEnabledFixture(t)
	traffic.mu.Lock()
	traffic.state.CaptureState = "passthrough"
	traffic.mu.Unlock()
	if _, err := service.Disable(context.Background()); err != nil {
		t.Fatalf("Disable() from passthrough error = %v", err)
	}
	if traffic.pauseCalls != 1 || traffic.releaseCalls != 1 || recovery.checkpoints[len(recovery.checkpoints)-1].DurablePhase != DurableInactive {
		t.Fatalf("passthrough disable calls/state = pause %d release %d journal=%#v", traffic.pauseCalls, traffic.releaseCalls, recovery.checkpoints[len(recovery.checkpoints)-1])
	}
}

func newFinishedFixture(t *testing.T) (*Service, *fakeTraffic, *fakeConfig, *fakeRecovery) {
	t.Helper()
	service, traffic, cfg, recovery := newEnabledFixture(t)
	if _, err := service.Disable(context.Background()); err != nil {
		t.Fatalf("Disable fixture: %v", err)
	}
	return service, traffic, cfg, recovery
}

func TestFinishSuccessClosesRelayWithoutConfigWrite(t *testing.T) {
	service, traffic, cfg, recovery := newFinishedFixture(t)
	commits := cfg.commitCalls
	got, err := service.Finish(context.Background(), false)
	if err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if got.Operation != OperationFinish || got.Phase != PhaseInactive || got.TrafficMode != trafficanalysis.ModeIdle || got.CaptureState != "stopped" || got.IntegrationActive {
		t.Fatalf("Finish result = %#v", got)
	}
	if cfg.commitCalls != commits || traffic.closeCalls != 1 || traffic.mappingPresent || traffic.state.Mode != trafficanalysis.ModeIdle || traffic.state.CaptureState != "stopped" {
		t.Fatalf("Finish changed config/traffic unexpectedly: commits %d/%d close %d state %#v", commits, cfg.commitCalls, traffic.closeCalls, traffic.state)
	}
	last := recovery.checkpoints[len(recovery.checkpoints)-1]
	if last.Phase != PhaseInactive || last.DurablePhase != DurableInactive || last.RelayActive || last.CaptureState != "stopped" || last.IntegrationActive {
		t.Fatalf("Finish checkpoint = %#v", last)
	}
}

func TestFinishRejectsActiveIntegrationAndDesktopOwnership(t *testing.T) {
	service, traffic, cfg, _ := newEnabledFixture(t)
	_, err := service.Finish(context.Background(), false)
	requireKind(t, err, KindFinishPrecondition)
	if traffic.closeCalls != 0 || cfg.commitCalls != 0 || traffic.state.Mode != trafficanalysis.ModeDesktop {
		t.Fatalf("active finish changed state: close=%d commits=%d mode=%q", traffic.closeCalls, cfg.commitCalls, traffic.state.Mode)
	}
}

func TestFinishRequiresExplicitUnsavedDiscard(t *testing.T) {
	service, traffic, _, recovery := newFinishedFixture(t)
	recovery.checkpoints[len(recovery.checkpoints)-1].UnsavedObservationsMayRemain = true
	_, err := service.Finish(context.Background(), false)
	if err == nil {
		t.Fatal("Finish should require confirmation for unsaved observations")
	}
	finishErr, ok := err.(*Error)
	if !ok || finishErr.Kind != KindFinishConfirmation || !finishErr.ConfirmationRequired {
		t.Fatalf("Finish error = %#v", err)
	}
	if traffic.closeCalls != 0 || traffic.state.Mode != trafficanalysis.ModeCaptureOnly || traffic.state.CaptureState != "passthrough" {
		t.Fatalf("confirmation changed traffic: close=%d state=%#v", traffic.closeCalls, traffic.state)
	}
	if _, err := service.Finish(context.Background(), true); err != nil {
		t.Fatalf("confirmed Finish() error = %v", err)
	}
	if len(recovery.checkpoints) < 2 || !recovery.checkpoints[len(recovery.checkpoints)-2].UnsavedObservationsMayRemain || !recovery.checkpoints[len(recovery.checkpoints)-2].UnsavedDiscardConfirmed {
		t.Fatalf("discard evidence was not durable before Close: %#v", recovery.checkpoints)
	}
	last := recovery.checkpoints[len(recovery.checkpoints)-1]
	if !last.UnsavedDiscardConfirmed || !last.UnsavedObservationsMayRemain || !last.AutoLogFinalized {
		t.Fatalf("discard evidence = %#v", last)
	}
}

func TestFinishCloseFailurePreservesConfigAndRequiresRecovery(t *testing.T) {
	service, traffic, cfg, recovery := newFinishedFixture(t)
	commits := cfg.commitCalls
	traffic.closeErr = errors.New("close failed")
	_, err := service.Finish(context.Background(), false)
	requireKind(t, err, KindFinishCloseFailed)
	if cfg.commitCalls != commits || traffic.closeCalls != 1 || traffic.state.Mode != trafficanalysis.ModeCaptureOnly {
		t.Fatalf("close failure changed state: commits=%d/%d close=%d mode=%q", commits, cfg.commitCalls, traffic.closeCalls, traffic.state.Mode)
	}
	if recovery.checkpoints[len(recovery.checkpoints)-1].DurablePhase != DurableReconciliationRequired {
		t.Fatalf("close failure checkpoint = %#v", recovery.checkpoints[len(recovery.checkpoints)-1])
	}
}

func TestFinishFinalCheckpointFailureDoesNotRestartOrReclaim(t *testing.T) {
	service, traffic, cfg, recovery := newFinishedFixture(t)
	commits := cfg.commitCalls
	recovery.checkpoints[len(recovery.checkpoints)-1].UnsavedObservationsMayRemain = true
	recovery.failOnCall = len(recovery.checkpoints) + 2
	_, err := service.Finish(context.Background(), true)
	requireKind(t, err, KindFinishFinalValidation)
	if cfg.commitCalls != commits || traffic.closeCalls != 1 || traffic.state.Mode != trafficanalysis.ModeIdle || traffic.state.CaptureState != "stopped" || traffic.claimCalls != 1 {
		t.Fatalf("final checkpoint failure changed state: commits=%d/%d close=%d claim=%d state=%#v", commits, cfg.commitCalls, traffic.closeCalls, traffic.claimCalls, traffic.state)
	}
	last := recovery.checkpoints[len(recovery.checkpoints)-1]
	if last.DurablePhase != DurableReconciliationRequired || !last.UnsavedObservationsMayRemain || !last.UnsavedDiscardConfirmed {
		t.Fatalf("final checkpoint failure evidence = %#v", recovery.checkpoints[len(recovery.checkpoints)-1])
	}
}

func TestFinishRejectsStaleCaptureBeforeClose(t *testing.T) {
	service, traffic, _, _ := newFinishedFixture(t)
	traffic.mu.Lock()
	traffic.state.GatewayAddress = "127.0.0.1:39999"
	traffic.mu.Unlock()
	_, err := service.Finish(context.Background(), false)
	requireKind(t, err, KindFinishPrecondition)
	if traffic.closeCalls != 0 {
		t.Fatalf("stale Finish closed capture: %d", traffic.closeCalls)
	}
}

func TestFinishOperationSlotConflict(t *testing.T) {
	service, traffic, _, _ := newFinishedFixture(t)
	if err := service.reserveOperation("other", OperationDisable); err != nil {
		t.Fatalf("reserve operation: %v", err)
	}
	_, err := service.Finish(context.Background(), false)
	requireKind(t, err, KindTransactionInProgress)
	service.releaseOperation("other")
	if traffic.closeCalls != 0 {
		t.Fatalf("conflicting Finish closed capture: %d", traffic.closeCalls)
	}
}

func TestFinishClassifierMatrix(t *testing.T) {
	if got := ClassifyFinishFailure(FinishFailureUnsavedConfirm); !got.ConfirmationRequired || got.ErrorKind != KindFinishConfirmation {
		t.Fatalf("unsaved classifier = %#v", got)
	}
	for _, failure := range []FinishFailure{FinishFailureClose, FinishFailureCloseUnknown, FinishFailureFinalCheckpoint, FinishFailureFinalValidation} {
		got := ClassifyFinishFailure(failure)
		if !got.RecoveryRequired || !got.Retryable {
			t.Fatalf("failure %q classifier = %#v", failure, got)
		}
	}
}
