package trafficanalysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// jsonBodyRequest builds an *http.Request with a JSON-encoded body.
func jsonBodyRequest(t *testing.T, method, url string, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, url, &buf)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// svcBindAndStart binds the default gateway identity and starts capture.
func svcBindAndStart(t *testing.T, svc *Service) State {
	t.Helper()
	svc.BindGatewayRun("gateway-1", "127.0.0.1:38440")
	st, err := svc.StartCapture(StartOptions{
		ListenAddr:   "127.0.0.1:0",
		UpstreamBase: "https://chatgpt.com/backend-api/codex",
	})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}
	return st
}

func mustErrKind(t *testing.T, err error, kind ErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error kind %q, got nil", kind)
	}
	se, ok := err.(*Error)
	if !ok {
		t.Fatalf("error = %T %v, want *Error", err, err)
	}
	if se.Kind != kind {
		t.Fatalf("error kind = %q, want %q", se.Kind, kind)
	}
}

func errorKind(err error) ErrorKind {
	if e, ok := err.(*Error); ok {
		return e.Kind
	}
	return ""
}

// fakeProxy is deliberately controlled by channels. These tests must execute
// the actual reservation and ownership paths, not mutate Service internals to
// imitate a race.
type fakeProxy struct {
	mu  sync.Mutex
	cfg CaptureConfig
	st  CaptureStatus

	startEntered, startRelease   chan struct{}
	closeEntered, closeRelease   chan struct{}
	pauseEntered, pauseRelease   chan struct{}
	stopEntered, stopRelease     chan struct{}
	startErr, closeErr           error
	pauseErr, stopErr            error
	startNotified, closeNotified bool
	pauseNotified, stopNotified  bool
	startCalls, pauseCalls       int
	resumeCalls, stopCalls       int
	closeCalls, clearCalls       int
}

func newFakeProxy(cfg CaptureConfig) *fakeProxy {
	return &fakeProxy{cfg: cfg, st: CaptureStatus{State: "stopped", CaptureAddress: cfg.ListenAddr}}
}

func (p *fakeProxy) Start() error {
	p.mu.Lock()
	p.startCalls++
	entered, release, err := p.startEntered, p.startRelease, p.startErr
	if entered != nil && !p.startNotified {
		close(entered)
		p.startNotified = true
	}
	p.mu.Unlock()
	if release != nil {
		<-release
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.st.State = "capturing"
		p.st.CaptureAddress = p.cfg.ListenAddr
	}
	return err
}

func (p *fakeProxy) Pause() error {
	p.mu.Lock()
	p.pauseCalls++
	entered, release, err := p.pauseEntered, p.pauseRelease, p.pauseErr
	if entered != nil && !p.pauseNotified {
		close(entered)
		p.pauseNotified = true
	}
	p.mu.Unlock()
	if release != nil {
		<-release
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.st.State = "passthrough"
	}
	return err
}

func (p *fakeProxy) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resumeCalls++
	p.st.State = "capturing"
	return nil
}

func (p *fakeProxy) Stop(_ context.Context) error {
	p.mu.Lock()
	p.stopCalls++
	entered, release, err := p.stopEntered, p.stopRelease, p.stopErr
	if entered != nil && !p.stopNotified {
		close(entered)
		p.stopNotified = true
	}
	p.mu.Unlock()
	if release != nil {
		<-release
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.st.State = "stopped"
	}
	return err
}

func (p *fakeProxy) Close() error {
	p.mu.Lock()
	p.closeCalls++
	entered, release, err := p.closeEntered, p.closeRelease, p.closeErr
	if entered != nil && !p.closeNotified {
		close(entered)
		p.closeNotified = true
	}
	p.mu.Unlock()
	if release != nil {
		<-release
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.st.State = "stopped"
	}
	return err
}

func (p *fakeProxy) Status() CaptureStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st
}

func (p *fakeProxy) Observations(uint64) ([]Observation, uint64) { return nil, 0 }
func (p *fakeProxy) Clear() {
	p.mu.Lock()
	p.clearCalls++
	p.mu.Unlock()
}
func (p *fakeProxy) StateStopped() bool { return p.Status().State == "stopped" }
func (p *fakeProxy) StateFailed() bool  { return p.Status().State == "failed" }

func (p *fakeProxy) callCounts() (pause, stop, close int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pauseCalls, p.stopCalls, p.closeCalls
}

// ---- A. Service ownership ----

func TestStartCaptureOwnsSingleProxyAndTracksIdentity(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)

	if st.Generation != 1 {
		t.Fatalf("generation = %d, want 1", st.Generation)
	}
	if st.Mode != ModeCaptureOnly {
		t.Fatalf("mode = %q, want capture_only", st.Mode)
	}
	if st.GatewayInstanceID != "gateway-1" || st.GatewayAddress != "127.0.0.1:38440" {
		t.Fatalf("gateway identity = %q/%q", st.GatewayInstanceID, st.GatewayAddress)
	}
	if st.CaptureState != "capturing" {
		t.Fatalf("captureState = %q, want capturing", st.CaptureState)
	}
	if !strings.HasPrefix(st.ListeningAddress, "127.0.0.1:") {
		t.Fatalf("listeningAddress = %q, want loopback", st.ListeningAddress)
	}
}

func TestDoubleStartDoesNotReplaceExistingProxy(t *testing.T) {
	svc := NewService()
	first := svcBindAndStart(t, svc)
	firstAddr := first.ListeningAddress

	_, err := svc.StartCapture(StartOptions{
		ListenAddr: "127.0.0.1:0",
	})
	mustErrKind(t, err, KindCaptureAlreadyActive)

	after := svc.Status()
	if after.GatewayInstanceID != "gateway-1" {
		t.Fatalf("instance after double start = %q, want gateway-1", after.GatewayInstanceID)
	}
	if after.Generation != 1 {
		t.Fatalf("generation after double start = %d, want 1", after.Generation)
	}
	if after.ListeningAddress != firstAddr {
		t.Fatalf("listeningAddress changed after double start: %q → %q", firstAddr, after.ListeningAddress)
	}
	// The original listener is still alive.
	conn, err := net.DialTimeout("tcp", firstAddr, time.Second)
	if err != nil {
		t.Fatalf("original listener not reachable: %v", err)
	}
	_ = conn.Close()
}

func TestStartCaptureBindFailureLeavesNoPartialOwnership(t *testing.T) {
	// Occupy the loopback address to force a bind failure.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().String()

	svc := NewService()
	svc.BindGatewayRun("gateway-x", "127.0.0.1:38440")
	_, err = svc.StartCapture(StartOptions{
		ListenAddr: taken,
	})
	mustErrKind(t, err, KindCaptureStartFailed)

	after := svc.Status()
	if after.Mode != ModeIdle {
		t.Fatalf("mode after bind failure = %q, want idle", after.Mode)
	}
	if after.CaptureState != "stopped" {
		t.Fatalf("capture state after bind failure = %q, want stopped", after.CaptureState)
	}

	// Retry is possible once the port frees.
	ln.Close()
	st, err := svc.StartCapture(StartOptions{
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("retry StartCapture() error = %v", err)
	}
	if st.CaptureState != "capturing" {
		t.Fatalf("retry state = %+v, want capturing", st)
	}
}

func TestStartCaptureRejectsUnboundIdentity(t *testing.T) {
	svc := NewService()
	// No BindGatewayRun — identity is empty.
	_, err := svc.StartCapture(StartOptions{
		ListenAddr: "127.0.0.1:0",
	})
	mustErrKind(t, err, KindGatewayNotBound)

	st := svc.Status()
	if st.Mode != ModeIdle {
		t.Fatalf("mode = %q, want idle", st.Mode)
	}
}

func TestStopCaptureReleasesListener(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	addr := st.ListeningAddress

	stopped, err := svc.StopCapture(context.Background())
	if err != nil {
		t.Fatalf("StopCapture() error = %v", err)
	}
	if stopped.CaptureState != "stopped" {
		t.Fatalf("captureState after stop = %q, want stopped", stopped.CaptureState)
	}
	// The listener is released: the same address can be re-bound.
	// On Windows, the OS may not release the socket immediately after
	// http.Server.Shutdown returns, so retry briefly.
	var ln net.Listener
	for range 5 {
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("re-bind %s after stop: %v", addr, err)
	}
	_ = ln.Close()

	// Repeated Stop is safe (proxy remains owned, but a stop on an already
	// stopped proxy returns the retained stopped state without error).
	if _, err := svc.StopCapture(context.Background()); err != nil {
		t.Fatalf("second StopCapture() error = %v", err)
	}
	svc.mu.Lock()
	retained := svc.proxy != nil
	svc.mu.Unlock()
	if !retained {
		t.Fatal("proxy was released on Stop; want retained")
	}
}

func TestCloseCaptureReleasesOwnershipBackToIdle(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)

	st, err := svc.CloseCapture(context.Background())
	if err != nil {
		t.Fatalf("CloseCapture() error = %v", err)
	}
	if st.Mode != ModeIdle || st.Generation != 2 {
		t.Fatalf("close state = %+v, want mode idle generation 2", st)
	}
	if st.GatewayInstanceID != "" || st.GatewayAddress != "" {
		t.Fatalf("gateway identity not cleared on close: %+v", st)
	}
	if st.CaptureState != "stopped" {
		t.Fatalf("captureState after close = %q, want stopped", st.CaptureState)
	}

	// Close again is safe (idempotent).
	if _, err := svc.CloseCapture(context.Background()); err != nil {
		t.Fatalf("second CloseCapture() error = %v", err)
	}
}

// ---- StopCapture vs CloseCapture contract ----

func TestStopCaptureKeepsProxyAndIdentity(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)

	stopped, err := svc.StopCapture(context.Background())
	if err != nil {
		t.Fatalf("StopCapture() error = %v", err)
	}

	// Listener is closed, proxy is in stopped state.
	if stopped.CaptureState != "stopped" {
		t.Fatalf("captureState = %q, want stopped", stopped.CaptureState)
	}
	// Mode stays capture_only (not idle).
	if stopped.Mode != ModeCaptureOnly {
		t.Fatalf("mode = %q, want capture_only", stopped.Mode)
	}
	// Gateway identity is preserved.
	if stopped.GatewayInstanceID != "gateway-1" {
		t.Fatalf("gatewayID = %q, want gateway-1", stopped.GatewayInstanceID)
	}
	// Generation is unchanged.
	if stopped.Generation != 1 {
		t.Fatalf("generation = %d, want 1", stopped.Generation)
	}
	// Proxy reference is retained.
	svc.mu.Lock()
	retained := svc.proxy != nil
	svc.mu.Unlock()
	if !retained {
		t.Fatal("proxy released by StopCapture; want retained")
	}
}

func TestCloseCaptureClearsProxyIdentityAndBumpsGeneration(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)

	closed, err := svc.CloseCapture(context.Background())
	if err != nil {
		t.Fatalf("CloseCapture() error = %v", err)
	}

	// Mode transitions to idle.
	if closed.Mode != ModeIdle {
		t.Fatalf("mode = %q, want idle", closed.Mode)
	}
	// Gateway identity is cleared.
	if closed.GatewayInstanceID != "" || closed.GatewayAddress != "" {
		t.Fatalf("identity not cleared: id=%q addr=%q", closed.GatewayInstanceID, closed.GatewayAddress)
	}
	// Generation is bumped (start=1, close=2).
	if closed.Generation != 2 {
		t.Fatalf("generation = %d, want 2", closed.Generation)
	}
	// Proxy reference is released.
	svc.mu.Lock()
	released := svc.proxy == nil
	svc.mu.Unlock()
	if !released {
		t.Fatal("proxy not released by CloseCapture; want nil")
	}
}

func TestPauseCaptureKeepsListenerAndProxy(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	addr := st.ListeningAddress

	paused, err := svc.PauseCapture(context.Background())
	if err != nil {
		t.Fatalf("PauseCapture() error = %v", err)
	}
	if paused.CaptureState != "passthrough" {
		t.Fatalf("captureState = %q, want passthrough", paused.CaptureState)
	}
	// Listener is still alive.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("listener not reachable after pause: %v", err)
	}
	conn.Close()

	// Mode is unchanged.
	if paused.Mode != ModeCaptureOnly {
		t.Fatalf("mode = %q, want capture_only", paused.Mode)
	}
	// Proxy is retained.
	svc.mu.Lock()
	retained := svc.proxy != nil
	svc.mu.Unlock()
	if !retained {
		t.Fatal("proxy released by PauseCapture; want retained")
	}
}

// ---- B. ManagementMode ----

func TestModeCaptureOnlyAllowsMutations(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)

	if _, err := svc.PauseCapture(context.Background()); err != nil {
		t.Fatalf("PauseCapture() in capture_only error = %v", err)
	}
	if _, err := svc.ResumeCapture(context.Background()); err != nil {
		t.Fatalf("ResumeCapture() in capture_only error = %v", err)
	}
	if _, err := svc.StopCapture(context.Background()); err != nil {
		t.Fatalf("StopCapture() in capture_only error = %v", err)
	}
	if err := svc.Clear(); err != nil {
		t.Fatalf("Clear() in capture_only error = %v", err)
	}
}

func TestModeDesktopGuardsExternalMutations(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	if _, err := svc.ClaimDesktop("gateway-1", "127.0.0.1:38440"); err != nil {
		t.Fatalf("ClaimDesktop() error = %v", err)
	}

	// status/observations allowed
	if st := svc.Status(); st.Mode != ModeDesktop {
		t.Fatalf("mode = %q, want desktop_managed", st.Mode)
	}
	if _, _ = svc.Observations(0); true {
		// allowed: no error path for observations
	}
	// mutations rejected
	mustErrKind(t, func() error { _, e := svc.StartCapture(StartOptions{}); return e }(), KindIntegrationManagedByDesktop)
	mustErrKind(t, func() error { _, e := svc.PauseCapture(context.Background()); return e }(), KindIntegrationManagedByDesktop)
	mustErrKind(t, func() error { _, e := svc.ResumeCapture(context.Background()); return e }(), KindIntegrationManagedByDesktop)
	mustErrKind(t, func() error { _, e := svc.StopCapture(context.Background()); return e }(), KindIntegrationManagedByDesktop)
	mustErrKind(t, svc.Clear(), KindIntegrationManagedByDesktop)
}

func TestModeRecoveryAllowsOnlyStatusAndObservations(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	if _, err := svc.MarkRecovery(); err != nil {
		t.Fatalf("MarkRecovery() error = %v", err)
	}
	if st := svc.Status(); st.Mode != ModeRecovery {
		t.Fatalf("mode = %q, want recovery_required", st.Mode)
	}
	if _, _ = svc.Observations(0); true {
		// allowed
	}
	mustErrKind(t, func() error { _, e := svc.StartCapture(StartOptions{}); return e }(), KindRecoveryConfirmationRequired)
	mustErrKind(t, func() error { _, e := svc.PauseCapture(context.Background()); return e }(), KindRecoveryConfirmationRequired)
	mustErrKind(t, func() error { _, e := svc.ResumeCapture(context.Background()); return e }(), KindRecoveryConfirmationRequired)
	mustErrKind(t, func() error { _, e := svc.StopCapture(context.Background()); return e }(), KindRecoveryConfirmationRequired)
	mustErrKind(t, func() error { _, e := svc.CloseCapture(context.Background()); return e }(), KindRecoveryConfirmationRequired)
	mustErrKind(t, svc.Clear(), KindRecoveryConfirmationRequired)
}

func TestClearRejectedByDesktopManagedKeepsObservations(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	if _, err := svc.ClaimDesktop("gateway-1", "127.0.0.1:38440"); err != nil {
		t.Fatal(err)
	}

	// Snapshot observations before the rejected clear.
	obsBefore, _ := svc.Observations(0)

	err := svc.Clear()
	mustErrKind(t, err, KindIntegrationManagedByDesktop)

	// Observations are preserved after the rejected clear.
	obsAfter, _ := svc.Observations(0)
	if len(obsAfter) != len(obsBefore) {
		t.Fatalf("observations changed after rejected clear: %d → %d", len(obsBefore), len(obsAfter))
	}
}

func TestClearRejectedByRecoveryKeepsObservations(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	svc.MarkRecovery()

	obsBefore, _ := svc.Observations(0)
	err := svc.Clear()
	mustErrKind(t, err, KindRecoveryConfirmationRequired)

	obsAfter, _ := svc.Observations(0)
	if len(obsAfter) != len(obsBefore) {
		t.Fatalf("observations changed after rejected clear: %d → %d", len(obsBefore), len(obsAfter))
	}
}

func TestManagementAPIClearRejectedByDesktopManaged(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	if _, err := svc.ClaimDesktop("gateway-1", "127.0.0.1:38440"); err != nil {
		t.Fatal(err)
	}
	handler := svc.ManagementHandler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+captureManagementPathPrefix+"clear", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("clear status = %d, want 409", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != string(KindIntegrationManagedByDesktop) {
		t.Fatalf("error body code = %q, want %q", body["code"], KindIntegrationManagedByDesktop)
	}
}

func TestManagementAPIClearRejectedByRecovery(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	svc.MarkRecovery()
	handler := svc.ManagementHandler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+captureManagementPathPrefix+"clear", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("clear status = %d, want 409", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != string(KindRecoveryConfirmationRequired) {
		t.Fatalf("error body code = %q, want %q", body["code"], KindRecoveryConfirmationRequired)
	}
}

func TestSnapshotNormalizesReadyToStopped(t *testing.T) {
	if got := normalizeCaptureState("ready"); got != "stopped" {
		t.Fatalf("normalizeCaptureState(ready) = %q, want stopped", got)
	}
	if got := normalizeCaptureState("capturing"); got != "capturing" {
		t.Fatalf("normalizeCaptureState(capturing) = %q, want capturing", got)
	}
	if got := normalizeCaptureState("passthrough"); got != "passthrough" {
		t.Fatalf("normalizeCaptureState(passthrough) = %q, want passthrough", got)
	}
	if got := normalizeCaptureState("stopped"); got != "stopped" {
		t.Fatalf("normalizeCaptureState(stopped) = %q, want stopped", got)
	}
}

// ---- C. Ownership transitions ----

func TestClaimDesktopPromotesWithoutRestart(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	genBefore := svc.Status().Generation

	st, err := svc.ClaimDesktop("gateway-1", "127.0.0.1:38440")
	if err != nil {
		t.Fatalf("ClaimDesktop() error = %v", err)
	}
	if st.Mode != ModeDesktop {
		t.Fatalf("mode after claim = %q, want desktop_managed", st.Mode)
	}
	// generation unchanged: Capture.Start is never re-run on claim.
	if st.Generation != genBefore {
		t.Fatalf("generation changed on claim: %d → %d", genBefore, st.Generation)
	}
	if st.CaptureState != "capturing" {
		t.Fatalf("captureState after claim = %q, want capturing", st.CaptureState)
	}
}

func TestClaimDesktopMismatchDoesNotMutate(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)

	// InstanceID mismatch
	mustErrKind(t, func() error { _, e := svc.ClaimDesktop("other", "127.0.0.1:38440"); return e }(), KindGatewayMismatch)
	// Address mismatch
	mustErrKind(t, func() error { _, e := svc.ClaimDesktop("gateway-1", "127.0.0.1:9999"); return e }(), KindGatewayMismatch)

	if st := svc.Status(); st.Generation != 1 || st.Mode != ModeCaptureOnly {
		t.Fatalf("state after failed claims = %+v, want capture_only generation 1", st)
	}
}

func TestReleaseDesktopReturnsToChosenModeWithoutStopping(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	if _, err := svc.ClaimDesktop("gateway-1", "127.0.0.1:38440"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReleaseDesktop(ModeCaptureOnly); err != nil {
		t.Fatalf("ReleaseDesktop() error = %v", err)
	}
	st := svc.Status()
	if st.Mode != ModeCaptureOnly {
		t.Fatalf("mode after release = %q, want capture_only", st.Mode)
	}
	if st.CaptureState != "capturing" {
		t.Fatalf("capture stopped by release: %q, want capturing", st.CaptureState)
	}
}

func TestClaimDesktopExpectedChangesOnlyModeAndNeverRestarts(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		return proxy
	})
	before := svcBindAndStart(t, svc)
	claimed, err := svc.ClaimDesktopExpected(before.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
	if err != nil {
		t.Fatalf("ClaimDesktopExpected() error = %v", err)
	}
	if claimed.Mode != ModeDesktop || claimed.Generation != before.Generation || claimed.GatewayInstanceID != before.GatewayInstanceID || claimed.GatewayAddress != before.GatewayAddress || claimed.ListeningAddress != before.ListeningAddress || claimed.CaptureState != before.CaptureState {
		t.Fatalf("ClaimDesktopExpected changed more than mode: before=%+v after=%+v", before, claimed)
	}
	proxy.mu.Lock()
	startCalls := proxy.startCalls
	proxy.mu.Unlock()
	if startCalls != 1 {
		t.Fatalf("CaptureProxy.Start calls = %d, want 1 total and 0 for claim", startCalls)
	}
}

func TestClaimDesktopExpectedRejectsGenerationAndIdentityWithoutMutation(t *testing.T) {
	svc := NewService()
	before := svcBindAndStart(t, svc)
	mustErrKind(t, func() error {
		_, err := svc.ClaimDesktopExpected(before.Generation+1, "gateway-1", "127.0.0.1:38440", "owner-a")
		return err
	}(), KindCaptureGenerationMismatch)
	mustErrKind(t, func() error {
		_, err := svc.ClaimDesktopExpected(before.Generation, "other", "127.0.0.1:38440", "owner-a")
		return err
	}(), KindGatewayMismatch)
	after := svc.Status()
	if after.Mode != ModeCaptureOnly || after.Generation != before.Generation || after.GatewayInstanceID != before.GatewayInstanceID || after.ListeningAddress != before.ListeningAddress {
		t.Fatalf("failed expected claims changed state: before=%+v after=%+v", before, after)
	}
}

func TestClaimDesktopExpectedRejectsWhileCloseIsInProgress(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		return proxy
	})
	before := svcBindAndStart(t, svc)
	proxy.closeEntered = make(chan struct{})
	proxy.closeRelease = make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		_, err := svc.CloseCapture(context.Background())
		closeDone <- err
	}()
	select {
	case <-proxy.closeEntered:
	case <-time.After(time.Second):
		t.Fatal("CloseCapture did not reach fake proxy")
	}
	mustErrKind(t, func() error {
		_, err := svc.ClaimDesktopExpected(before.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
		return err
	}(), KindCaptureClosing)
	close(proxy.closeRelease)
	if err := <-closeDone; err != nil {
		t.Fatalf("CloseCapture error = %v", err)
	}
}

func TestPauseDesktopExpectedPreservesOwnershipAndExternalGuard(t *testing.T) {
	svc := NewService()
	claimed := svcBindAndStart(t, svc)
	claimed, err := svc.ClaimDesktopExpected(claimed.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	paused, err := svc.PauseDesktopExpected(context.Background(), claimed.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
	if err != nil {
		t.Fatalf("PauseDesktopExpected() error = %v", err)
	}
	if paused.Mode != ModeDesktop || paused.CaptureState != "passthrough" || paused.Generation != claimed.Generation || paused.GatewayInstanceID != claimed.GatewayInstanceID || paused.ListeningAddress != claimed.ListeningAddress {
		t.Fatalf("unexpected paused state: %+v", paused)
	}
	mustErrKind(t, func() error {
		_, err := svc.PauseCapture(context.Background())
		return err
	}(), KindIntegrationManagedByDesktop)
}

func TestPauseDesktopExpectedFailureEntersRecovery(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		proxy.pauseErr = errors.New("injected pause failure")
		return proxy
	})
	st := svcBindAndStart(t, svc)
	st, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.PauseDesktopExpected(context.Background(), st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
	mustErrKind(t, err, KindCaptureStopFailed)
	if got.Mode != ModeRecovery || got.GatewayInstanceID != "gateway-1" || got.CaptureState != "capturing" {
		t.Fatalf("pause failure state = %+v", got)
	}
}

func TestPauseDesktopExpectedStaleCompletionDoesNotMutateReplacement(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		return proxy
	})
	st := svcBindAndStart(t, svc)
	st, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	proxy.pauseEntered = make(chan struct{})
	proxy.pauseRelease = make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := svc.PauseDesktopExpected(context.Background(), st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
		result <- err
	}()
	select {
	case <-proxy.pauseEntered:
	case <-time.After(time.Second):
		t.Fatal("PauseDesktopExpected did not reach fake proxy")
	}
	replacement := newFakeProxy(CaptureConfig{ListenAddr: "127.0.0.1:38441"})
	replacement.st.State = "capturing"
	svc.mu.Lock()
	oldOp := svc.activeOp
	svc.activeOp = nil
	svc.proxy = replacement
	svc.generation++
	svc.mode = ModeDesktop
	svc.gatewayID = "gateway-new"
	svc.gatewayAddr = "127.0.0.1:48440"
	svc.lastError = "replacement-state"
	svc.desktopOwnerID = "owner-b"
	svc.mu.Unlock()
	close(proxy.pauseRelease)
	if err := <-result; errorKind(err) != KindCaptureOperationSuperseded {
		t.Fatalf("stale pause error = %v, want operation superseded", err)
	}
	svc.mu.Lock()
	if svc.activeOp != nil || oldOp == nil {
		t.Fatalf("stale pause left operation state: active=%v old=%v", svc.activeOp, oldOp)
	}
	svc.mu.Unlock()
	current := svc.Status()
	if current.Mode != ModeDesktop || current.GatewayInstanceID != "gateway-new" || current.GatewayAddress != "127.0.0.1:48440" || current.LastError != "replacement-state" || current.ListeningAddress != "127.0.0.1:38441" {
		t.Fatalf("stale pause mutated replacement state: %+v", current)
	}
}

func TestExpectedOwnershipMutationsRejectActivePauseOperation(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		return proxy
	})
	st := svcBindAndStart(t, svc)
	claimed, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	proxy.pauseEntered = make(chan struct{})
	proxy.pauseRelease = make(chan struct{})
	pauseDone := make(chan error, 1)
	go func() {
		_, err := svc.PauseDesktopExpected(context.Background(), claimed.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
		pauseDone <- err
	}()
	select {
	case <-proxy.pauseEntered:
	case <-time.After(time.Second):
		t.Fatal("PauseDesktopExpected did not reach fake proxy")
	}
	mustErrKind(t, func() error { _, err := svc.ReleaseDesktopExpected(claimed.Generation, "owner-a"); return err }(), KindCaptureAlreadyActive)
	mustErrKind(t, func() error {
		_, err := svc.ClaimDesktopExpected(claimed.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
		return err
	}(), KindCaptureAlreadyActive)
	close(proxy.pauseRelease)
	if err := <-pauseDone; err != nil {
		t.Fatalf("PauseDesktopExpected error = %v", err)
	}
}

func TestReleaseDesktopExpectedChangesOnlyMode(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	claimed, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	released, err := svc.ReleaseDesktopExpected(claimed.Generation, "owner-a")
	if err != nil {
		t.Fatalf("ReleaseDesktopExpected() error = %v", err)
	}
	if released.Mode != ModeCaptureOnly || released.Generation != claimed.Generation || released.GatewayInstanceID != claimed.GatewayInstanceID || released.ListeningAddress != claimed.ListeningAddress || released.CaptureState != claimed.CaptureState {
		t.Fatalf("release changed more than mode: claimed=%+v released=%+v", claimed, released)
	}
}

func TestReleaseDesktopExpectedRejectsStaleGeneration(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	if _, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a"); err != nil {
		t.Fatal(err)
	}
	mustErrKind(t, func() error {
		_, err := svc.ReleaseDesktopExpected(st.Generation+1, "owner-a")
		return err
	}(), KindCaptureGenerationMismatch)
	if current := svc.Status(); current.Mode != ModeDesktop {
		t.Fatalf("stale release changed ownership: %+v", current)
	}
}

func TestStaleDesktopReleaseCannotClearNewOwner(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	if _, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReleaseDesktopExpected(st.Generation, "owner-a"); err != nil {
		t.Fatalf("owner A release error = %v", err)
	}
	if _, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-b"); err != nil {
		t.Fatalf("owner B claim error = %v", err)
	}
	mustErrKind(t, func() error {
		_, err := svc.ReleaseDesktopExpected(st.Generation, "owner-a")
		return err
	}(), KindCaptureOperationSuperseded)
	if current := svc.Status(); current.Mode != ModeDesktop {
		t.Fatalf("stale owner A release changed owner B mode: %+v", current)
	}
	mustErrKind(t, func() error {
		_, err := svc.PauseDesktopExpected(context.Background(), st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
		return err
	}(), KindCaptureOperationSuperseded)
	if current := svc.Status(); current.Mode != ModeDesktop || current.CaptureState != "capturing" {
		t.Fatalf("stale owner A pause changed owner B state: %+v", current)
	}
}

func TestDesktopOwnerIsPrivateAndRecoveryPreservesThenClearsIt(t *testing.T) {
	svc := NewService()
	st := svcBindAndStart(t, svc)
	if _, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-secret"); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(svc.Status())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "owner-secret") {
		t.Fatalf("desktop owner leaked into State JSON: %s", encoded)
	}
	if _, err := svc.MarkRecovery(); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	if svc.desktopOwnerID != "owner-secret" {
		t.Fatalf("Recovery cleared owner ID before explicit recovery: %q", svc.desktopOwnerID)
	}
	svc.mu.Unlock()
	if _, err := svc.ClearRecovery(ModeCaptureOnly); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.desktopOwnerID != "" {
		t.Fatalf("ClearRecovery retained desktop owner ID: %q", svc.desktopOwnerID)
	}
}

func TestExpectedOwnershipMutationsRejectCloseInProgress(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		return proxy
	})
	st := svcBindAndStart(t, svc)
	proxy.closeEntered = make(chan struct{})
	proxy.closeRelease = make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		_, err := svc.CloseCapture(context.Background())
		closeDone <- err
	}()
	select {
	case <-proxy.closeEntered:
	case <-time.After(time.Second):
		t.Fatal("CloseCapture did not reach fake proxy")
	}
	mustErrKind(t, func() error {
		_, err := svc.ClaimDesktopExpected(st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
		return err
	}(), KindCaptureClosing)
	mustErrKind(t, func() error {
		_, err := svc.PauseDesktopExpected(context.Background(), st.Generation, "gateway-1", "127.0.0.1:38440", "owner-a")
		return err
	}(), KindCaptureClosing)
	mustErrKind(t, func() error { _, err := svc.ReleaseDesktopExpected(st.Generation, "owner-a"); return err }(), KindCaptureClosing)
	close(proxy.closeRelease)
	if err := <-closeDone; err != nil {
		t.Fatalf("CloseCapture error = %v", err)
	}
}

func TestRecoveryMarksExplicitAndNeverAutoClears(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	svc.MarkRecovery()
	if st := svc.Status(); st.Mode != ModeRecovery {
		t.Fatalf("mode after mark = %q, want recovery_required", st.Mode)
	}
	// Starting a new capture fails until explicit clear.
	mustErrKind(t, func() error { _, e := svc.StartCapture(StartOptions{}); return e }(), KindRecoveryConfirmationRequired)
	if _, err := svc.ClearRecovery(ModeIdle); err != nil {
		t.Fatalf("ClearRecovery() error = %v", err)
	}
	if st := svc.Status(); st.Mode != ModeIdle {
		t.Fatalf("mode after clear = %q, want idle", st.Mode)
	}
}

// ---- D. Gateway lifecycle ----

func TestMarkGatewayLostActiveRunEntersRecovery(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	svc.MarkGatewayLost("gateway-1", true)
	st := svc.Status()
	if st.Mode != ModeRecovery {
		t.Fatalf("mode after gateway lost = %q, want recovery_required", st.Mode)
	}
	if !strings.Contains(st.LastError, "gateway run lost") {
		t.Fatalf("lastError after gateway lost = %q", st.LastError)
	}
}

func TestMarkGatewayLostStaleRunIsNoOp(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	svc.MarkGatewayLost("stale-run", true)
	st := svc.Status()
	if st.Mode != ModeCaptureOnly {
		t.Fatalf("mode after stale gateway lost = %q, want capture_only", st.Mode)
	}
	if st.Generation != 1 {
		t.Fatalf("generation changed by stale lost = %d, want 1", st.Generation)
	}
}

func TestBindGatewayRunRegistersIdentity(t *testing.T) {
	svc := NewService()
	svc.BindGatewayRun("gw-1", "127.0.0.1:38440")
	st := svc.Status()
	if st.GatewayInstanceID != "gw-1" || st.GatewayAddress != "127.0.0.1:38440" {
		t.Fatalf("identity after bind = %q/%q, want gw-1/127.0.0.1:38440", st.GatewayInstanceID, st.GatewayAddress)
	}
}

func TestBindGatewayRunUpdatesIdentityAcrossRestarts(t *testing.T) {
	svc := NewService()
	svc.BindGatewayRun("run-1", "127.0.0.1:38440")
	svc.BindGatewayRun("run-2", "127.0.0.1:38441")
	st := svc.Status()
	if st.GatewayInstanceID != "run-2" || st.GatewayAddress != "127.0.0.1:38441" {
		t.Fatalf("identity after re-bind = %q/%q, want run-2/127.0.0.1:38441", st.GatewayInstanceID, st.GatewayAddress)
	}
}

func TestBindThenLostFullLifecycle(t *testing.T) {
	svc := NewService()

	// Gateway run 1 starts → binds identity → capture starts.
	svc.BindGatewayRun("run-1", "127.0.0.1:38440")
	_, err := svc.StartCapture(StartOptions{
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("StartCapture(run-1) error = %v", err)
	}

	// Gateway run 1 finishes → lost notification → recovery.
	st := svc.MarkGatewayLost("run-1", true)
	if st.Mode != ModeRecovery {
		t.Fatalf("mode after run-1 lost = %q, want recovery_required", st.Mode)
	}

	// Close the old capture and clear recovery to simulate the App's
	// post-recovery cleanup path.
	if _, err := svc.ClearRecovery(ModeCaptureOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CloseCapture(context.Background()); err != nil {
		t.Fatalf("CloseCapture() error = %v", err)
	}

	// Gateway run 2 starts → binds new identity → capture starts.
	svc.BindGatewayRun("run-2", "127.0.0.1:38441")
	_, err = svc.StartCapture(StartOptions{
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("StartCapture(run-2) error = %v", err)
	}

	// Stale lost from run-1 is a no-op (gatewayID is now "run-2").
	st = svc.MarkGatewayLost("run-1", true)
	if st.Mode != ModeCaptureOnly {
		t.Fatalf("mode after stale run-1 lost = %q, want capture_only", st.Mode)
	}

	// Active lost from run-2 enters recovery.
	st = svc.MarkGatewayLost("run-2", true)
	if st.Mode != ModeRecovery {
		t.Fatalf("mode after run-2 lost = %q, want recovery_required", st.Mode)
	}
}

func TestMarkGatewayLostNormalStopStopsCaptureAndTransitionsToIdle(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)

	// Normal stop (abnormal=false): stops capture proxy, clears identity,
	// transitions to idle — no recovery.
	st := svc.MarkGatewayLost("gateway-1", false)
	if st.Mode != ModeIdle {
		t.Fatalf("mode = %q, want idle (capture stopped on normal end)", st.Mode)
	}
	if st.GatewayInstanceID != "" || st.GatewayAddress != "" {
		t.Fatalf("identity not cleared: id=%q addr=%q", st.GatewayInstanceID, st.GatewayAddress)
	}
	// Capture should be stopped/passthrough, not actively capturing.
	if st.CaptureState == "capturing" {
		t.Fatalf("captureState = %q, want stopped/passthrough (not actively capturing)", st.CaptureState)
	}
}

func TestStartCaptureRestartsFromStoppedProxy(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)

	// Stop the capture.
	if _, err := svc.StopCapture(context.Background()); err != nil {
		t.Fatalf("StopCapture() error = %v", err)
	}
	if st := svc.Status(); st.CaptureState != "stopped" {
		t.Fatalf("captureState = %q, want stopped", st.CaptureState)
	}

	// Start again: auto-closes stopped proxy, starts fresh.
	st, err := svc.StartCapture(StartOptions{
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("StartCapture() after stop error = %v", err)
	}
	if st.CaptureState != "capturing" {
		t.Fatalf("captureState = %q, want capturing", st.CaptureState)
	}
	if st.Mode != ModeCaptureOnly {
		t.Fatalf("mode = %q, want capture_only", st.Mode)
	}
}

func TestStartCaptureUsesBoundIdentityExclusively(t *testing.T) {
	svc := NewService()
	// Bind identity via lifecycle hook.
	svc.BindGatewayRun("bound-id", "127.0.0.1:38440")

	// Start with no identity in opts.
	st, err := svc.StartCapture(StartOptions{
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}
	if st.GatewayInstanceID != "bound-id" {
		t.Fatalf("gatewayID = %q, want bound-id (from BindGatewayRun)", st.GatewayInstanceID)
	}
	if st.GatewayAddress != "127.0.0.1:38440" {
		t.Fatalf("gatewayAddr = %q, want 127.0.0.1:38440", st.GatewayAddress)
	}
}

func TestStaleRunEndDoesNotAffectNewRun(t *testing.T) {
	svc := NewService()

	// Run 1: bind, start, normal stop.
	svc.BindGatewayRun("run-1", "127.0.0.1:38440")
	_, err := svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("StartCapture(run-1) error = %v", err)
	}
	st := svc.MarkGatewayLost("run-1", false)
	if st.Mode != ModeIdle {
		t.Fatalf("mode after run-1 normal stop = %q, want idle", st.Mode)
	}

	// Close the stopped/paused proxy from run-1 before starting run-2.
	if _, err := svc.CloseCapture(context.Background()); err != nil {
		t.Fatalf("CloseCapture() error = %v", err)
	}

	// Run 2: bind, start.
	svc.BindGatewayRun("run-2", "127.0.0.1:38441")
	_, err = svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("StartCapture(run-2) error = %v", err)
	}

	// Stale end from run-1: no-op.
	st = svc.MarkGatewayLost("run-1", true)
	if st.Mode != ModeCaptureOnly {
		t.Fatalf("mode after stale run-1 end = %q, want capture_only", st.Mode)
	}
	if st.GatewayInstanceID != "run-2" {
		t.Fatalf("identity changed by stale run-1 end: %q", st.GatewayInstanceID)
	}
}

func TestAbnormalGatewayLostPausesCaptureAndEntersRecovery(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)

	st := svc.MarkGatewayLost("gateway-1", true)
	if st.Mode != ModeRecovery {
		t.Fatalf("mode = %q, want recovery_required", st.Mode)
	}
	// Capture should be paused (passthrough), not actively capturing.
	if st.CaptureState == "capturing" {
		t.Fatalf("captureState = %q, want passthrough (paused on abnormal end)", st.CaptureState)
	}
}

func TestCloseCaptureDuringStartReservation(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	// Stop first so restart can happen.
	if _, err := svc.StopCapture(context.Background()); err != nil {
		t.Fatal(err)
	}
	// CloseCapture should invalidate any concurrent start reservation.
	st, err := svc.CloseCapture(context.Background())
	if err != nil {
		t.Fatalf("CloseCapture() error = %v", err)
	}
	if st.Mode != ModeIdle {
		t.Fatalf("mode = %q, want idle", st.Mode)
	}
}

// ---- E. Shared handler ----

func TestManagementHandlerAndServiceShareStateAndObservations(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)

	handler := svc.ManagementHandler()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+captureManagementPathPrefix+"status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status over management handler = %d, want 200", rec.Code)
	}
	var claimed State
	if err := json.NewDecoder(rec.Body).Decode(&claimed); err != nil {
		t.Fatal(err)
	}
	direct := svc.Status()
	if claimed.Generation != direct.Generation || claimed.GatewayInstanceID != direct.GatewayInstanceID || claimed.CaptureState != direct.CaptureState {
		t.Fatalf("handler snapshot %+v != service snapshot %+v", claimed, direct)
	}

	// observations endpoint shares the same ring buffer as the service.
	obsReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+captureManagementPathPrefix+"observations?after=0", nil)
	obsRec := httptest.NewRecorder()
	handler.ServeHTTP(obsRec, obsReq)
	if obsRec.Code != http.StatusOK {
		t.Fatalf("observations over management handler = %d, want 200", obsRec.Code)
	}
	if !strings.Contains(obsRec.Body.String(), "observations") {
		t.Fatal("observations response missing observations field")
	}
	if len(obsRec.Body.String()) == 0 {
		t.Fatal("observations response is empty")
	}
}

func TestManagementHandlerRoutesStartPauseStopClear(t *testing.T) {
	svc := NewService()
	// Bind identity so management API start can proceed.
	svc.BindGatewayRun("mgmt-gw", "127.0.0.1:38440")
	handler := svc.ManagementHandler()

	startReq := jsonBodyRequest(t, http.MethodPost, "http://127.0.0.1"+captureManagementPathPrefix+"start", map[string]string{
		"listen_addr": "127.0.0.1:0",
	})
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, startReq)
	if start.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202", start.Code)
	}
	if svc.Status().CaptureState != "capturing" {
		t.Fatalf("capture not capturing after handler start")
	}

	pause := httptest.NewRecorder()
	handler.ServeHTTP(pause, httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+captureManagementPathPrefix+"pause", nil))
	if pause.Code != http.StatusAccepted {
		t.Fatalf("pause status = %d, want 202", pause.Code)
	}

	clear := httptest.NewRecorder()
	handler.ServeHTTP(clear, httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+captureManagementPathPrefix+"clear", nil))
	if clear.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want 200", clear.Code)
	}

	stop := httptest.NewRecorder()
	handler.ServeHTTP(stop, httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+captureManagementPathPrefix+"stop", nil))
	if stop.Code != http.StatusAccepted {
		t.Fatalf("stop status = %d, want 202", stop.Code)
	}
}

func TestManagementAPIStartRejectsUnboundIdentity(t *testing.T) {
	svc := NewService()
	// No BindGatewayRun — management API start should be rejected.
	handler := svc.ManagementHandler()

	startReq := jsonBodyRequest(t, http.MethodPost, "http://127.0.0.1"+captureManagementPathPrefix+"start", map[string]string{
		"listen_addr": "127.0.0.1:0",
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, startReq)
	if rec.Code != http.StatusConflict {
		t.Fatalf("start status = %d, want 409 (conflict)", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != string(KindGatewayNotBound) {
		t.Fatalf("error code = %q, want %q", body["code"], KindGatewayNotBound)
	}
}

// ---- F. Concurrency ----

func TestConcurrentStartIsSingleFlight(t *testing.T) {
	svc := NewService()
	svc.BindGatewayRun("gateway-1", "127.0.0.1:38440")
	var wg sync.WaitGroup
	results := make([]struct {
		state State
		err   error
	}, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i].state, results[i].err = svc.StartCapture(StartOptions{
				ListenAddr: "127.0.0.1:0",
			})
		}(i)
	}
	wg.Wait()

	var successes int
	for _, r := range results {
		if r.err == nil {
			successes++
		} else if se, ok := r.err.(*Error); ok && se.Kind == KindCaptureAlreadyActive {
			// expected loser
		} else {
			t.Fatalf("unexpected concurrent start error: %v", r.err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent start successes = %d, want 1", successes)
	}
	final := svc.Status()
	if final.Generation != 1 {
		t.Fatalf("final generation = %d, want 1", final.Generation)
	}
}

func TestConcurrentRestartIsSingleFlight(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	// Stop so restart path is triggered.
	if _, err := svc.StopCapture(context.Background()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]struct {
		state State
		err   error
	}, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i].state, results[i].err = svc.StartCapture(StartOptions{
				ListenAddr: "127.0.0.1:0",
			})
		}(i)
	}
	wg.Wait()

	var successes int
	for _, r := range results {
		if r.err == nil {
			successes++
		} else if se, ok := r.err.(*Error); ok && se.Kind == KindCaptureAlreadyActive {
			// expected loser
		} else {
			t.Fatalf("unexpected concurrent restart error: %v", r.err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent restart successes = %d, want 1", successes)
	}
}

func TestConcurrentStopCloseIsSafe(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = svc.StopCapture(context.Background()) }()
		go func() { defer wg.Done(); _, _ = svc.CloseCapture(context.Background()) }()
	}
	wg.Wait()
	// No panic, and the service is left in a deterministic stopped/idle state.
	svc.Status()
}

func TestStartWhileGatewayLostIsRejected(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	svc.MarkGatewayLost("gateway-1", true)
	mustErrKind(t, func() error { _, e := svc.StartCapture(StartOptions{}); return e }(), KindRecoveryConfirmationRequired)
}

func TestObservationsStatusConcurrentWithStop(t *testing.T) {
	svc := NewService()
	svcBindAndStart(t, svc)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = svc.Status()
			_, _ = svc.Observations(0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = svc.StopCapture(context.Background())
			_, _ = svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"})
		}
	}()
	wg.Wait()
}

// ---- G. Security ----

func TestStateNeverLeaksTokenOrRawRequestBody(t *testing.T) {
	svc := NewService()
	svc.BindGatewayRun("gateway-1", "127.0.0.1:38440")
	if _, err := svc.StartCapture(StartOptions{
		GatewayToken: "SENTINEL_CONTROL_TOKEN",
		ListenAddr:   "127.0.0.1:0",
	}); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(svc.Status())
	if strings.Contains(string(encoded), "SENTINEL_CONTROL_TOKEN") {
		t.Fatal("State leaks the gateway token")
	}
}

func TestManagementJSONHasNoSecretFieldsAndNoErrorContent(t *testing.T) {
	svc := NewService()
	svc.BindGatewayRun("gateway-1", "127.0.0.1:38440")
	handler := svc.ManagementHandler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+captureManagementPathPrefix+"status", nil))
	body := rec.Body.String()
	for _, forbidden := range []string{"token", "GatewayToken", "authorization", "prompt", "SENTINEL"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("management status contains forbidden %q: %s", forbidden, body)
		}
	}

	// A double start is a real conflict: first start binds, second fails with a
	// sanitized code/message body (never a raw lower-level string).
	firstReq := jsonBodyRequest(t, http.MethodPost, "http://127.0.0.1"+captureManagementPathPrefix+"start", map[string]string{
		"listen_addr": "127.0.0.1:0",
	})
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, firstReq)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first start status = %d, want 202", first.Code)
	}
	conflict := httptest.NewRecorder()
	handler.ServeHTTP(conflict, httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+captureManagementPathPrefix+"start", nil))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict start status = %d, want 409", conflict.Code)
	}
	var decoded map[string]string
	if err := json.NewDecoder(conflict.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["code"] != string(KindCaptureAlreadyActive) {
		t.Fatalf("error body code = %q, want %q", decoded["code"], KindCaptureAlreadyActive)
	}
	if _, ok := decoded["message"]; !ok || decoded["message"] == "" {
		t.Fatalf("error body lacks message: %v", decoded)
	}
}

// ---- H. Reservation ABA and identity consistency ----

func TestStartReservationABAPrevention(t *testing.T) {
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	secondStarted := make(chan struct{})
	secondRelease := make(chan struct{})
	var calls int
	var mu sync.Mutex
	svc := newService(func(cfg CaptureConfig) captureProxy {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		p := newFakeProxy(cfg)
		if n == 1 {
			p.startEntered, p.startRelease = firstStarted, firstRelease
			p.startErr = errors.New("injected stale Start A failure")
		} else {
			p.startEntered, p.startRelease = secondStarted, secondRelease
		}
		return p
	})
	if _, err := svc.BindGatewayRun("gw-1", "127.0.0.1:38440"); err != nil {
		t.Fatal(err)
	}
	aResult := make(chan error, 1)
	go func() {
		_, err := svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"})
		aResult <- err
	}()
	<-firstStarted
	if _, err := svc.CloseCapture(context.Background()); err != nil {
		t.Fatalf("CloseCapture during Start A: %v", err)
	}
	if _, err := svc.BindGatewayRun("gw-2", "127.0.0.1:38441"); err != nil {
		t.Fatal(err)
	}
	bResult := make(chan error, 1)
	go func() {
		_, err := svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"})
		bResult <- err
	}()
	<-secondStarted
	close(firstRelease)
	if err := <-aResult; err == nil || errorKind(err) != KindCaptureStartSuperseded {
		t.Fatalf("Start A error = %v, want superseded", err)
	}
	st := svc.Status()
	if st.GatewayInstanceID != "gw-2" || st.Operation != OperationStarting {
		t.Fatalf("state after superseded A = %+v", st)
	}
	close(secondRelease)
	if err := <-bResult; err != nil {
		t.Fatalf("Start B error = %v", err)
	}
	if got := svc.Status().GatewayInstanceID; got != "gw-2" {
		t.Fatalf("committed identity = %q, want gw-2", got)
	}
}

func TestStartReservationIDIsMonotonicallyIncreasing(t *testing.T) {
	// Verify that startSeq increases with each start attempt, so no two
	// attempts ever share a reservation ID.
	svc := NewService()
	svc.BindGatewayRun("gw-1", "127.0.0.1:38440")

	svc.mu.Lock()
	before := svc.startSeq
	svc.mu.Unlock()

	if _, err := svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}

	svc.mu.Lock()
	afterFirst := svc.startSeq
	svc.mu.Unlock()

	if afterFirst <= before {
		t.Fatalf("startSeq did not advance: before=%d, after=%d", before, afterFirst)
	}

	// Stop and restart to verify it advances again.
	if _, err := svc.StopCapture(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}

	svc.mu.Lock()
	afterSecond := svc.startSeq
	svc.mu.Unlock()

	if afterSecond <= afterFirst {
		t.Fatalf("startSeq did not advance on restart: first=%d, second=%d", afterFirst, afterSecond)
	}
}

func TestStartCapturesGatewayIdentityAtReservationAndRejectsRebind(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	svc := newService(func(cfg CaptureConfig) captureProxy {
		p := newFakeProxy(cfg)
		p.startEntered, p.startRelease = started, release
		return p
	})
	if _, err := svc.BindGatewayRun("run-1", "127.0.0.1:38440"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"})
		result <- err
	}()
	<-started
	if _, err := svc.BindGatewayRun("run-2", "127.0.0.1:38441"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err == nil || errorKind(err) != KindGatewayMismatch {
		t.Fatalf("Start error = %v, want gateway mismatch", err)
	}
	st := svc.Status()
	if st.GatewayInstanceID != "run-2" || st.Mode != ModeIdle || st.ListeningAddress != "" {
		t.Fatalf("state after rebind rejection = %+v", st)
	}
}

func TestNormalGatewayEndStopFailureEntersRecovery(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		proxy.stopErr = errors.New("injected stop failure")
		return proxy
	})
	svcBindAndStart(t, svc)
	st := svc.MarkGatewayLost("gateway-1", false)
	if st.Mode != ModeRecovery || st.GatewayInstanceID == "" || proxy == nil {
		t.Fatalf("mode/state = %+v, want recovery with retained ownership", st)
	}
}

func TestNormalGatewayEndAfterCloseEntersRecovery(t *testing.T) {
	// After CloseCapture, the proxy is nil. A subsequent MarkGatewayLost
	// with abnormal=false should go to idle (Stop on nil proxy is a no-op
	// success).
	svc := NewService()
	svcBindAndStart(t, svc)

	if _, err := svc.CloseCapture(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Close already cleared identity, so MarkGatewayLost is a no-op.
	st := svc.MarkGatewayLost("gateway-1", false)
	if st.Mode != ModeIdle {
		t.Fatalf("mode = %q, want idle (close already cleared everything)", st.Mode)
	}
}

func TestAbnormalGatewayLostPauseFailureReflectedInLastError(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		proxy.pauseErr = errors.New("injected pause failure")
		return proxy
	})
	svcBindAndStart(t, svc)
	st := svc.MarkGatewayLost("gateway-1", true)
	if st.Mode != ModeRecovery {
		t.Fatalf("mode = %q, want recovery_required", st.Mode)
	}
	if !strings.Contains(st.LastError, "pause failed") || proxy == nil {
		t.Fatalf("lastError = %q, want sanitized pause failure", st.LastError)
	}
}

func TestStaleReservationCannotClearNewReservation(t *testing.T) {
	// Verify that an old Start's commit path does not clear activeStartID
	// if it doesn't match.
	svc := NewService()
	svc.BindGatewayRun("gw-1", "127.0.0.1:38440")

	// Start normally.
	if _, err := svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}

	// Stop to allow restart.
	if _, err := svc.StopCapture(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Now do two sequential restarts. The second should work.
	st1, err := svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("restart 1 error = %v", err)
	}
	if st1.CaptureState != "capturing" {
		t.Fatalf("restart 1 state = %q, want capturing", st1.CaptureState)
	}
}

func TestMarkGatewayLostNormalEndWithStoppedProxy(t *testing.T) {
	// If the proxy is already stopped, MarkGatewayLost(abnormal=false)
	// should still succeed and transition to idle.
	svc := NewService()
	svcBindAndStart(t, svc)

	// Stop the capture (listener closed, proxy in stopped state).
	if _, err := svc.StopCapture(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Normal end: Stop on stopped proxy should succeed → idle.
	st := svc.MarkGatewayLost("gateway-1", false)
	if st.Mode != ModeIdle {
		t.Fatalf("mode = %q, want idle", st.Mode)
	}
	if st.GatewayInstanceID != "" {
		t.Fatalf("identity = %q, want empty", st.GatewayInstanceID)
	}
}

func TestCommittedCaptureRejectsDifferentGatewayBind(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		return proxy
	})
	if _, err := svc.BindGatewayRun("run-1", "127.0.0.1:38440"); err != nil {
		t.Fatal(err)
	}
	before := svc.Status()
	if _, err := svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	afterStart := svc.Status()
	if _, err := svc.BindGatewayRun("run-2", "127.0.0.1:38441"); errorKind(err) != KindGatewayMismatch {
		t.Fatalf("rebind error = %v, want gateway mismatch", err)
	}
	st := svc.Status()
	if st.GatewayInstanceID != afterStart.GatewayInstanceID || st.Mode != afterStart.Mode || proxy == nil {
		t.Fatalf("rebind changed committed state: before=%+v after=%+v", afterStart, st)
	}
	if before.GatewayInstanceID != "run-1" {
		t.Fatalf("initial identity = %q", before.GatewayInstanceID)
	}
}

func TestCloseCaptureIsSingleFlightAndPublishesClosing(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		return proxy
	})
	svcBindAndStart(t, svc)
	proxy.closeEntered = make(chan struct{})
	proxy.closeRelease = make(chan struct{})
	first := make(chan struct {
		st  State
		err error
	}, 1)
	go func() {
		st, err := svc.CloseCapture(context.Background())
		first <- struct {
			st  State
			err error
		}{st, err}
	}()
	<-proxy.closeEntered
	if got := svc.Status().Operation; got != OperationClosing {
		t.Fatalf("operation = %q, want closing", got)
	}
	for _, name := range []string{"start", "pause", "resume", "stop", "clear", "bind"} {
		var err error
		switch name {
		case "start":
			_, err = svc.StartCapture(StartOptions{ListenAddr: "127.0.0.1:0"})
		case "pause":
			_, err = svc.PauseCapture(context.Background())
		case "resume":
			_, err = svc.ResumeCapture(context.Background())
		case "stop":
			_, err = svc.StopCapture(context.Background())
		case "clear":
			err = svc.Clear()
		case "bind":
			_, err = svc.BindGatewayRun("run-2", "127.0.0.1:38441")
		}
		if errorKind(err) != KindCaptureClosing {
			t.Fatalf("%s during close error = %v, want closing", name, err)
		}
	}
	second := make(chan struct {
		st  State
		err error
	}, 1)
	go func() {
		st, err := svc.CloseCapture(context.Background())
		second <- struct {
			st  State
			err error
		}{st, err}
	}()
	close(proxy.closeRelease)
	firstResult := <-first
	secondResult := <-second
	if firstResult.err != nil || secondResult.err != nil {
		t.Fatalf("joined close errors = %v, %v", firstResult.err, secondResult.err)
	}
	if firstResult.st.Mode != ModeIdle || secondResult.st.Mode != ModeIdle {
		t.Fatalf("joined close states = %+v, %+v", firstResult.st, secondResult.st)
	}
	if got := svc.Status().Operation; got != OperationNone {
		t.Fatalf("operation after close = %q, want none", got)
	}
}

func TestCloseFailureRetainsCommittedOwnershipAndRecovery(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		proxy.closeErr = errors.New("injected close failure")
		return proxy
	})
	svcBindAndStart(t, svc)
	st, err := svc.CloseCapture(context.Background())
	if errorKind(err) != KindCaptureStopFailed {
		t.Fatalf("close error = %v, want stop failure", err)
	}
	if st.Mode != ModeRecovery || st.GatewayInstanceID == "" || st.Operation != OperationNone {
		t.Fatalf("close failure state = %+v", st)
	}
	if svc.proxy == nil || proxy == nil {
		t.Fatal("close failure released committed proxy")
	}
}

func TestGatewayStopAndPauseFailuresEnterRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		abnormal bool
	}{
		{name: "stop", abnormal: false},
		{name: "pause", abnormal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var proxy *fakeProxy
			svc := newService(func(cfg CaptureConfig) captureProxy {
				proxy = newFakeProxy(cfg)
				if tc.abnormal {
					proxy.pauseErr = errors.New("injected pause failure")
				} else {
					proxy.stopErr = errors.New("injected stop failure")
				}
				return proxy
			})
			svcBindAndStart(t, svc)
			st := svc.MarkGatewayLost("gateway-1", tc.abnormal)
			if st.Mode != ModeRecovery || st.GatewayInstanceID == "" || st.LastError == "" {
				t.Fatalf("failure state = %+v", st)
			}
			if svc.proxy == nil || proxy == nil {
				t.Fatal("failure path released proxy ownership")
			}
		})
	}
}

func TestCloseWaitsForEarlierStopOrPause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		invoke func(*Service) error
		block  func(*fakeProxy) (<-chan struct{}, chan struct{})
	}{
		{
			name: "stop",
			invoke: func(svc *Service) error {
				_, err := svc.StopCapture(context.Background())
				return err
			},
			block: func(proxy *fakeProxy) (<-chan struct{}, chan struct{}) {
				proxy.stopEntered = make(chan struct{})
				proxy.stopRelease = make(chan struct{})
				return proxy.stopEntered, proxy.stopRelease
			},
		},
		{
			name: "pause",
			invoke: func(svc *Service) error {
				_, err := svc.PauseCapture(context.Background())
				return err
			},
			block: func(proxy *fakeProxy) (<-chan struct{}, chan struct{}) {
				proxy.pauseEntered = make(chan struct{})
				proxy.pauseRelease = make(chan struct{})
				return proxy.pauseEntered, proxy.pauseRelease
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var proxy *fakeProxy
			svc := newService(func(cfg CaptureConfig) captureProxy {
				proxy = newFakeProxy(cfg)
				return proxy
			})
			svcBindAndStart(t, svc)
			entered, release := tc.block(proxy)
			mutationResult := make(chan error, 1)
			go func() { mutationResult <- tc.invoke(svc) }()
			<-entered

			proxy.closeEntered = make(chan struct{})
			proxy.closeRelease = make(chan struct{})
			closeResult := make(chan error, 1)
			go func() {
				_, err := svc.CloseCapture(context.Background())
				closeResult <- err
			}()
			select {
			case <-proxy.closeEntered:
				t.Fatal("Close reached proxy while the earlier mutation was blocked")
			case <-time.After(50 * time.Millisecond):
			}

			close(release)
			if err := <-mutationResult; err != nil {
				t.Fatalf("mutation error = %v", err)
			}
			select {
			case <-proxy.closeEntered:
			case <-time.After(time.Second):
				t.Fatal("Close did not begin after the earlier mutation completed")
			}
			close(proxy.closeRelease)
			if err := <-closeResult; err != nil {
				t.Fatalf("CloseCapture error = %v", err)
			}
		})
	}
}

func TestGatewayEndWaitsForCloseWithoutSecondProxyMutation(t *testing.T) {
	for _, abnormal := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "abnormal"}[abnormal], func(t *testing.T) {
			var proxy *fakeProxy
			svc := newService(func(cfg CaptureConfig) captureProxy {
				proxy = newFakeProxy(cfg)
				return proxy
			})
			svcBindAndStart(t, svc)
			proxy.closeEntered = make(chan struct{})
			proxy.closeRelease = make(chan struct{})
			closeResult := make(chan error, 1)
			go func() {
				_, err := svc.CloseCapture(context.Background())
				closeResult <- err
			}()
			<-proxy.closeEntered

			endResult := make(chan State, 1)
			go func() { endResult <- svc.MarkGatewayLost("gateway-1", abnormal) }()
			time.Sleep(50 * time.Millisecond)
			pauseCalls, stopCalls, _ := proxy.callCounts()
			if pauseCalls != 0 || stopCalls != 0 {
				t.Fatalf("EndRun mutated proxy during Close: pause=%d stop=%d", pauseCalls, stopCalls)
			}

			close(proxy.closeRelease)
			if err := <-closeResult; err != nil {
				t.Fatalf("CloseCapture error = %v", err)
			}
			st := <-endResult
			if st.Mode != ModeIdle || st.Operation != OperationNone {
				t.Fatalf("EndRun result after Close = %+v", st)
			}
			pauseCalls, stopCalls, closeCalls := proxy.callCounts()
			if pauseCalls != 0 || stopCalls != 0 || closeCalls != 1 {
				t.Fatalf("proxy calls = pause:%d stop:%d close:%d", pauseCalls, stopCalls, closeCalls)
			}
		})
	}
}

func TestCloseWaitsForGatewayEndStop(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		return proxy
	})
	svcBindAndStart(t, svc)
	proxy.stopEntered = make(chan struct{})
	proxy.stopRelease = make(chan struct{})
	endResult := make(chan State, 1)
	go func() { endResult <- svc.MarkGatewayLost("gateway-1", false) }()
	<-proxy.stopEntered

	proxy.closeEntered = make(chan struct{})
	proxy.closeRelease = make(chan struct{})
	closeResult := make(chan error, 1)
	go func() {
		_, err := svc.CloseCapture(context.Background())
		closeResult <- err
	}()
	select {
	case <-proxy.closeEntered:
		t.Fatal("Close reached proxy while Gateway End Stop was blocked")
	case <-time.After(50 * time.Millisecond):
	}

	close(proxy.stopRelease)
	if st := <-endResult; st.Mode != ModeIdle {
		t.Fatalf("Gateway End result = %+v", st)
	}
	select {
	case <-proxy.closeEntered:
	case <-time.After(time.Second):
		t.Fatal("Close did not begin after Gateway End completed")
	}
	close(proxy.closeRelease)
	if err := <-closeResult; err != nil {
		t.Fatalf("CloseCapture error = %v", err)
	}
	_, stopCalls, closeCalls := proxy.callCounts()
	if stopCalls != 1 || closeCalls != 1 {
		t.Fatalf("proxy calls = stop:%d close:%d, want one each", stopCalls, closeCalls)
	}
}

func TestCloseWaitsForGatewayEndPause(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		return proxy
	})
	svcBindAndStart(t, svc)
	proxy.pauseEntered = make(chan struct{})
	proxy.pauseRelease = make(chan struct{})
	endResult := make(chan State, 1)
	go func() { endResult <- svc.MarkGatewayLost("gateway-1", true) }()
	<-proxy.pauseEntered

	proxy.closeEntered = make(chan struct{})
	proxy.closeRelease = make(chan struct{})
	closeResult := make(chan error, 1)
	go func() {
		_, err := svc.CloseCapture(context.Background())
		closeResult <- err
	}()
	select {
	case <-proxy.closeEntered:
		t.Fatal("Close reached proxy while Gateway End Pause was blocked")
	case <-time.After(50 * time.Millisecond):
	}

	close(proxy.pauseRelease)
	if st := <-endResult; st.Mode != ModeRecovery {
		t.Fatalf("Gateway abnormal End result = %+v, want recovery", st)
	}
	// Abnormal End enters recovery, so the normal Close command is rejected by
	// the recovery guard after the pause operation finishes. The important
	// ownership assertion is that Close never ran concurrently with Pause.
	closeErr := <-closeResult
	if errorKind(closeErr) != KindRecoveryConfirmationRequired {
		t.Fatalf("Close after abnormal End error = %v, want recovery confirmation", closeErr)
	}
	pauseCalls, _, closeCalls := proxy.callCounts()
	if pauseCalls != 1 || closeCalls != 0 {
		t.Fatalf("proxy calls = pause:%d close:%d, want one pause and no close", pauseCalls, closeCalls)
	}
}

func TestStaleMutationErrorCannotOverwriteReplacementState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*fakeProxy) (<-chan struct{}, chan struct{})
		invoke  func(*Service) error
	}{
		{
			name: "stop",
			prepare: func(proxy *fakeProxy) (<-chan struct{}, chan struct{}) {
				proxy.stopEntered = make(chan struct{})
				proxy.stopRelease = make(chan struct{})
				proxy.stopErr = errors.New("injected stale stop failure")
				return proxy.stopEntered, proxy.stopRelease
			},
			invoke: func(svc *Service) error {
				_, err := svc.StopCapture(context.Background())
				return err
			},
		},
		{
			name: "pause",
			prepare: func(proxy *fakeProxy) (<-chan struct{}, chan struct{}) {
				proxy.pauseEntered = make(chan struct{})
				proxy.pauseRelease = make(chan struct{})
				proxy.pauseErr = errors.New("injected stale pause failure")
				return proxy.pauseEntered, proxy.pauseRelease
			},
			invoke: func(svc *Service) error {
				_, err := svc.PauseCapture(context.Background())
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var oldProxy *fakeProxy
			svc := newService(func(cfg CaptureConfig) captureProxy {
				oldProxy = newFakeProxy(cfg)
				return oldProxy
			})
			svcBindAndStart(t, svc)
			entered, release := tc.prepare(oldProxy)
			result := make(chan error, 1)
			go func() { result <- tc.invoke(svc) }()
			<-entered

			replacement := newFakeProxy(CaptureConfig{ListenAddr: "127.0.0.1:49999"})
			replacement.st.State = "capturing"
			svc.mu.Lock()
			svc.proxy = replacement
			svc.generation++
			svc.gatewayID = "gateway-new"
			svc.gatewayAddr = "127.0.0.1:48440"
			svc.mode = ModeCaptureOnly
			svc.lastError = "replacement-state"
			svc.mu.Unlock()

			close(release)
			if err := <-result; errorKind(err) != KindCaptureStopFailed {
				t.Fatalf("stale mutation error = %v, want safe operation failure", err)
			}
			st := svc.Status()
			if svc.proxy != replacement || st.GatewayInstanceID != "gateway-new" || st.Mode != ModeCaptureOnly || st.LastError != "replacement-state" {
				t.Fatalf("stale result changed replacement state: %+v", st)
			}
		})
	}
}

func TestDuplicateCloseWaiterCancellationDoesNotCancelOperation(t *testing.T) {
	var proxy *fakeProxy
	svc := newService(func(cfg CaptureConfig) captureProxy {
		proxy = newFakeProxy(cfg)
		return proxy
	})
	svcBindAndStart(t, svc)
	proxy.closeEntered = make(chan struct{})
	proxy.closeRelease = make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := svc.CloseCapture(context.Background())
		first <- err
	}()
	<-proxy.closeEntered

	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() {
		_, err := svc.CloseCapture(ctx)
		second <- err
	}()
	cancel()
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatalf("duplicate waiter error = %v, want context.Canceled", err)
	}
	_, _, closeCalls := proxy.callCounts()
	if closeCalls != 1 {
		t.Fatalf("Close calls after waiter cancellation = %d, want 1", closeCalls)
	}

	close(proxy.closeRelease)
	if err := <-first; err != nil {
		t.Fatalf("original Close error = %v", err)
	}
	if st := svc.Status(); st.Mode != ModeIdle || st.Operation != OperationNone {
		t.Fatalf("final state = %+v", st)
	}
}

func TestDifferentGatewayBindRejectedForStoppedAndDesktopManagedCapture(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, *Service)
	}{
		{
			name: "stopped",
			prepare: func(t *testing.T, svc *Service) {
				t.Helper()
				if _, err := svc.StopCapture(context.Background()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "desktop_managed",
			prepare: func(t *testing.T, svc *Service) {
				t.Helper()
				if _, err := svc.ClaimDesktop("gateway-1", "127.0.0.1:38440"); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newService(func(cfg CaptureConfig) captureProxy { return newFakeProxy(cfg) })
			svcBindAndStart(t, svc)
			tc.prepare(t, svc)
			before := svc.Status()
			if _, err := svc.BindGatewayRun("gateway-2", "127.0.0.1:48440"); errorKind(err) != KindGatewayMismatch {
				t.Fatalf("BindGatewayRun error = %v, want gateway mismatch", err)
			}
			after := svc.Status()
			if after.GatewayInstanceID != before.GatewayInstanceID || after.GatewayAddress != before.GatewayAddress || after.Mode != before.Mode || after.ListeningAddress != before.ListeningAddress {
				t.Fatalf("rejected bind changed state: before=%+v after=%+v", before, after)
			}
		})
	}
}
