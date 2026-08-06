// Package gateway exposes the Moon Bridge gateway startup as a reusable
// application service so that both the CLI and the Wails Desktop shell run
// the same code, in-process, and can stop and restart it.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"moonbridge/internal/config"
	"moonbridge/internal/service/app"
	"moonbridge/internal/service/desktopcontrol"
)

// ServiceOptions holds the service-level configuration fixed at construction.
type ServiceOptions struct {
	// Errors is the output stream for runtime notices (CLI: stderr).
	Errors io.Writer
}

// StartOptions describes one gateway run. It is passed per Start so a
// restart can carry a fresh instance identity and an updated Config.
type StartOptions struct {
	Config      config.Config
	DesktopMode bool
	InstanceID  string // required when DesktopMode
	Token       string // required when DesktopMode
	ServerToken string // cfg.AuthToken, forwarded to /api/v1/* requests
	// Traffic is the App-owned traffic analysis surface mounted for the run.
	// It is forwarded into the run's app.RunOptions so runTransform mounts the
	// same management handler/status the external API uses. May be nil.
	Traffic app.TrafficProvider
	// TrafficLifecycle carries the callbacks the Gateway run uses to notify
	// the owning traffic Service of run start and finish events. The
	// callbacks are called without the gateway lock held. Token is never
	// forwarded through these callbacks. May be nil (no lifecycle hooks).
	TrafficLifecycle *app.TrafficLifecycle
}

// Status is the lifecycle state of a gateway run.
type Status string

const (
	StatusStopped  Status = "stopped"
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusStopping Status = "stopping"
	StatusFailed   Status = "failed"
)

// State is a snapshot of the service's public state.
type State struct {
	Status     Status      `json:"status"`
	Mode       config.Mode `json:"mode"`
	Addr       string      `json:"addr"` // resolved listen address (:0 is materialized)
	PID        int         `json:"pid"`
	InstanceID string      `json:"instanceId"`
	StartedAt  time.Time   `json:"startedAt"`
	LastError  string      `json:"lastError,omitempty"`
}

var (
	ErrAlreadyRunning              = errors.New("gateway: already running")
	ErrNotRunning                  = errors.New("gateway: not running")
	ErrStartCanceled               = errors.New("gateway: start canceled")
	ErrDesktopModeRequiresIdentity = errors.New("gateway: desktop mode requires instance id and token")
	ErrDesktopModeRequiresLoopback = errors.New("gateway: desktop mode requires a loopback listen address")
)

type startResult struct {
	state State
	err   error
}

// runState is the per-run bookkeeping. run.ctx is the run-scoped context;
// its cancellation is used to distinguish a cancel-before-bind from a real
// startup failure even when app.RunServerWithOptions returns nil.
type runState struct {
	id     uint64
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	err    error

	startupOnce sync.Once        // the startup outcome is reported exactly once
	started     chan startResult // buffered(1)

	// bindCalled is set by onListening after the listener binds successfully.
	// It gates EndRun: EndRun is only sent when bindCalled is true, so that
	// pre-bind failures never trigger traffic Service recovery.
	// Uses atomic.Bool because onListening (writer) and notifyTrafficEndSafe
	// (reader) may run on different goroutines depending on the runServer
	// implementation.
	bindCalled atomic.Bool
}

func (r *runState) completeStartup(state State, err error) {
	r.startupOnce.Do(func() {
		select {
		case r.started <- startResult{state: state, err: err}:
		default:
		}
	})
}

// runServerFunc starts the HTTP gateway for one run. It is a field on
// Service so white-box tests can inject a panicking runner and exercise the
// real recover path in runGoroutine.
type runServerFunc func(context.Context, config.Config, io.Writer, app.RunOptions) error

// Service runs one gateway at a time and supports stopping and restarting
// it in the same process.
type Service struct {
	opts      ServiceOptions
	runServer runServerFunc // 通常は app.RunServerWithOptions、テストで差し替え

	mu        sync.Mutex
	nextRunID uint64
	current   *runState // the starting/running/stopping run; nil when stopped/failed
	lastRun   *runState // most recent finished run, kept so Wait() can await it
	state     State
}

func NewService(opts ServiceOptions) *Service {
	return &Service{
		opts:      opts,
		runServer: app.RunServerWithOptions,
		state:     State{Status: StatusStopped},
	}
}

// NewDesktopIdentity returns a fresh instance identity for a desktop-hosted
// gateway run: a 32-hex instance id and its matching control token. Both are
// generated as UUIDv4 without dashes, matching the shape the Tauri shell uses.
func NewDesktopIdentity() (instanceID, token string) {
	instanceID = strings.ReplaceAll(uuid.NewString(), "-", "")
	token = strings.ReplaceAll(uuid.NewString(), "-", "")
	return instanceID, token
}

// Start validates the options, transitions to starting, and blocks until the
// listener is bound (running) or the startup fails/cancels.
func (s *Service) Start(ctx context.Context, opts StartOptions) (State, error) {
	s.mu.Lock()
	if s.current != nil {
		s.mu.Unlock()
		return s.state, ErrAlreadyRunning
	}
	if opts.DesktopMode {
		if opts.InstanceID == "" || opts.Token == "" {
			s.mu.Unlock()
			return s.state, ErrDesktopModeRequiresIdentity
		}
		if !IsLoopbackAddress(opts.Config.Addr) {
			s.mu.Unlock()
			return s.state, ErrDesktopModeRequiresLoopback
		}
	}
	s.nextRunID++
	runCtx, cancel := context.WithCancel(ctx)
	run := &runState{
		id:      s.nextRunID,
		ctx:     runCtx,
		cancel:  cancel,
		done:    make(chan struct{}),
		started: make(chan startResult, 1),
	}
	s.current = run
	s.state = State{
		Status:     StatusStarting,
		Mode:       opts.Config.Mode,
		PID:        os.Getpid(),
		InstanceID: opts.InstanceID,
		StartedAt:  time.Now().UTC(),
	}
	s.mu.Unlock()

	// BindRun is called inside runGoroutine (after the goroutine starts,
	// before the server runs) so that startup failures before the server
	// binds do not trigger recovery. See runGoroutine.

	go s.runGoroutine(run, opts)

	select {
	case res := <-run.started:
		return res.state, res.err
	case <-ctx.Done():
		// A bind that succeeded just before cancellation takes priority.
		select {
		case res := <-run.started:
			return res.state, res.err
		default:
		}
		return s.Status(), ErrStartCanceled
	}
}

// runGoroutine owns the run's lifecycle: it recovers panics into a failed
// state and reports the startup outcome exactly once.
//
// BindRun is NOT called here — it is called from onListening after the
// listener binds successfully. This ensures that pre-bind failures (port
// occupied, config error) never trigger BindRun or EndRun, and therefore
// never cause the traffic Service to enter recovery.
//
// The defer/recover is registered before any callback-invoking code so that
// panics in callbacks themselves are caught.
func (s *Service) runGoroutine(run *runState, opts StartOptions) {
	var runErr error
	defer func() {
		if rec := recover(); rec != nil {
			runErr = fmt.Errorf("gateway run panic: %v", rec)
			s.notifyTrafficEndSafe(run, opts, app.EndRunPanic)
			s.finishRun(run, runErr)
			run.completeStartup(s.stateSnapshot(), runErr)
		}
	}()
	runErr = s.run(run, opts)
	s.notifyTrafficEndSafe(run, opts, reasonFromError(runErr))
	// Publish the terminal run state only after EndRun has completed. Stop
	// waits on run.done; closing it earlier allowed a restart to begin while
	// the previous traffic ownership callback was still mutating its state.
	s.finishRun(run, runErr)
	run.completeStartup(s.classifyStartup(run, runErr))
}

// notifyTrafficEndSafe calls the TrafficLifecycle.EndRun callback only if
// bindCalled is true (the listener bound successfully). It isolates callback
// panics so they never propagate into the gateway cleanup path.
func (s *Service) notifyTrafficEndSafe(run *runState, opts StartOptions, reason app.EndRunReason) {
	if !run.bindCalled.Load() {
		return
	}
	if opts.TrafficLifecycle != nil && opts.TrafficLifecycle.EndRun != nil {
		func() {
			defer func() { recover() }()
			opts.TrafficLifecycle.EndRun(opts.InstanceID, reason)
		}()
	}
}

// reasonFromError maps a run error to an EndRunReason. A nil error means
// the run stopped normally.
func reasonFromError(err error) app.EndRunReason {
	if err != nil {
		return app.EndRunFailed
	}
	return app.EndRunStopped
}

// classifyStartup decides the startup outcome for a run that has finished.
// A canceled run context always reports ErrStartCanceled, even when the
// underlying server returned nil (graceful cancel path).
func (s *Service) classifyStartup(run *runState, runErr error) (State, error) {
	switch {
	case run.ctx.Err() != nil:
		return s.stateSnapshot(), ErrStartCanceled
	case runErr != nil:
		return s.stateSnapshot(), runErr
	default:
		return s.stateSnapshot(), nil
	}
}

func (s *Service) run(run *runState, opts StartOptions) error {
	var control *desktopcontrol.Control
	if opts.DesktopMode {
		control = desktopcontrol.New(opts.InstanceID, opts.Token, run.cancel).WithServerToken(opts.ServerToken)
	}
	return s.runServer(run.ctx, opts.Config, s.opts.Errors, app.RunOptions{
		DesktopControl: control,
		Traffic:        opts.Traffic,
		OnListening:    func(addr string) error { return s.onListening(run, opts, addr) },
	})
}

// onListening binds traffic ownership before publishing the Gateway as
// running. The listener has already been created by app; returning an error
// causes app to close it without starting HTTP serving.
func (s *Service) onListening(run *runState, opts StartOptions, addr string) (err error) {
	s.mu.Lock()
	if s.current != run || s.state.Status != StatusStarting {
		s.mu.Unlock()
		return ErrStartCanceled
	}
	s.mu.Unlock()

	// BindRun is called without the Gateway lock because the callback may
	// acquire the traffic Service lock. A callback panic is converted into a
	// sanitized startup error and does not count as a successful bind.
	if opts.TrafficLifecycle != nil && opts.TrafficLifecycle.BindRun != nil {
		err = func() (callbackErr error) {
			defer func() {
				if recover() != nil {
					callbackErr = errors.New("gateway traffic bind failed")
				}
			}()
			return opts.TrafficLifecycle.BindRun(opts.InstanceID, addr)
		}()
		if err != nil {
			return fmt.Errorf("gateway traffic bind failed")
		}
	}

	// The run may have been stopped while the callback was executing. A
	// successful bind is still owned and must receive normal EndRun cleanup,
	// but it must never publish a stale running state.
	run.bindCalled.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != run || s.state.Status != StatusStarting {
		return ErrStartCanceled
	}
	s.state.Status = StatusRunning
	s.state.Addr = addr
	run.completeStartup(s.state, nil)
	return nil
}

// finishRun records the terminal state and keeps the run as lastRun so that
// Wait() can await an already-finished run. Stale callbacks from a previous
// run are ignored.
func (s *Service) finishRun(run *runState, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != run {
		return
	}
	if err != nil {
		s.state.Status = StatusFailed
		s.state.LastError = err.Error()
	} else {
		s.state.Status = StatusStopped
	}
	run.err = err
	s.current = nil
	s.lastRun = run
	close(run.done)
}

// Stop cancels the current run and waits for it to finish. It is idempotent
// and a no-op when nothing is running.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	run := s.current
	if run == nil {
		s.mu.Unlock()
		return nil
	}
	if s.state.Status == StatusStarting || s.state.Status == StatusRunning {
		s.state.Status = StatusStopping
	}
	s.mu.Unlock()

	run.cancel()
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait blocks until the current run finishes, falling back to the most
// recent finished run so a run that ended between Start and Wait is still
// awaited. It returns ErrNotRunning only if the service was never started.
func (s *Service) Wait() error {
	s.mu.Lock()
	run := s.current
	if run == nil {
		run = s.lastRun
	}
	s.mu.Unlock()
	if run == nil {
		return ErrNotRunning
	}
	<-run.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return run.err
}

// Status returns a snapshot of the current service state.
func (s *Service) Status() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Service) stateSnapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Addr returns the resolved listen address of the current state.
func (s *Service) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Addr
}

// InstanceID returns the instance identity of the current state.
func (s *Service) InstanceID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.InstanceID
}

// IsLoopbackAddress reports whether addr is a tcp listen address on a
// loopback interface (127.0.0.1, ::1). Hostnames such as localhost and
// wildcard addresses are not loopback.
func IsLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
