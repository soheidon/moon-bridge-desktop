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
	"sync"
	"time"

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
func (s *Service) runGoroutine(run *runState, opts StartOptions) {
	var runErr error
	defer func() {
		if rec := recover(); rec != nil {
			runErr = fmt.Errorf("gateway run panic: %v", rec)
			s.finishRun(run, runErr)
			run.completeStartup(s.stateSnapshot(), runErr)
		}
	}()
	runErr = s.run(run, opts)
	s.finishRun(run, runErr)
	// Classify a run that ended before the listener bound. completeStartup
	// is guarded by sync.Once, so a bind that already won stays authoritative.
	run.completeStartup(s.classifyStartup(run, runErr))
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
		OnListening:    func(addr string) { s.onListening(run, addr) },
	})
}

// onListening transitions starting -> running, and only while the run is
// still current and starting. A delayed callback from a stopped run (or one
// that is already stopping) is ignored, so stopping can never regress to
// running.
func (s *Service) onListening(run *runState, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != run || s.state.Status != StatusStarting {
		return
	}
	s.state.Status = StatusRunning
	s.state.Addr = addr
	st := s.state
	run.completeStartup(st, nil)
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
