package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/logger"
	"moonbridge/internal/service/app"
)

// captureConfig returns a minimal CaptureAnthropic config that reaches the
// HTTP listener quickly (no provider/plugin/db bootstrap).
func captureConfig(t *testing.T, addr string) config.Config {
	t.Helper()
	return config.Config{
		Mode: config.ModeCaptureAnthropic,
		Addr: addr,
		AnthropicProxy: config.AnthropicProxyConfig{
			ProviderBaseURL: "https://api.example.invalid",
			ProviderAPIKey:  "test-key",
			ProviderVersion: "2023-06-01",
		},
	}
}

// transformConfig mirrors the app package's e2e config so that the full
// runTransform bootstrap (including the plugin log consumer) is exercised.
func transformConfig(t *testing.T, addr string, dbPath string) config.Config {
	t.Helper()
	enabled := true
	cfg, err := config.FromFileConfigWithOptions(config.FileConfig{
		Mode: string(config.ModeTransform),
		Server: config.ServerFileConfig{
			Addr: addr,
		},
		Defaults: config.DefaultsFileConfig{
			Model:     "moonbridge",
			MaxTokens: 1024,
		},
		Models: map[string]config.ModelDefFileConfig{
			"local-test-model": {
				ContextWindow:   128000,
				MaxOutputTokens: 4096,
			},
		},
		Providers: map[string]config.ProviderDefFileConfig{
			"local": {
				BaseURL:  "https://api.example.invalid",
				APIKey:   "test-key",
				Protocol: config.ProtocolOpenAIChat,
				Offers: []config.OfferFileConfig{
					{
						Model:        "local-test-model",
						UpstreamName: "local-test-model",
					},
				},
			},
		},
		Routes: map[string]config.RouteFileConfig{
			"moonbridge": {
				Provider: "local",
				Model:    "local-test-model",
			},
		},
		Persistence: config.PersistenceFileConfig{
			ActiveProvider: "db_sqlite",
		},
		Extensions: map[string]config.ExtensionFileConfig{
			"db_sqlite": {
				Enabled: &enabled,
				Config: map[string]any{
					"path": dbPath,
					"wal":  false,
				},
			},
		},
	}, config.LoadOptions{ExtensionSpecs: app.BuiltinExtensions().ConfigSpecs()})
	if err != nil {
		t.Fatalf("build transform config: %v", err)
	}
	return cfg
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free loopback addr: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close free loopback listener: %v", err)
	}
	return addr
}

func stopService(t *testing.T, svc *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestServiceLifecycle(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	st, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if st.Status != StatusRunning {
		t.Fatalf("Start() status = %v, want running", st.Status)
	}
	if _, err := net.ResolveTCPAddr("tcp", st.Addr); err != nil {
		t.Fatalf("Start() addr %q is not a resolved address: %v", st.Addr, err)
	}
	if svc.Addr() != st.Addr {
		t.Fatalf("Addr() = %q, want %q", svc.Addr(), st.Addr)
	}
	if got := svc.Status().Status; got != StatusRunning {
		t.Fatalf("Status() = %v, want running", got)
	}

	stopService(t, svc)

	if got := svc.Status().Status; got != StatusStopped {
		t.Fatalf("Status() after Stop = %v, want stopped", got)
	}
	if svc.Status().LastError != "" {
		t.Fatalf("LastError = %q, want empty after clean stop", svc.Status().LastError)
	}
}

func TestServiceRestartPortZero(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})

	st1, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	})
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if st1.Status != StatusRunning {
		t.Fatalf("first Start() status = %v, want running", st1.Status)
	}
	stopService(t, svc)

	// A :0 restart must bind a valid address; the plan does not require the
	// same port to be reused.
	st2, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	})
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if st2.Status != StatusRunning {
		t.Fatalf("second Start() status = %v, want running", st2.Status)
	}
	if _, err := net.ResolveTCPAddr("tcp", st2.Addr); err != nil {
		t.Fatalf("second Start() addr %q is not a resolved address: %v", st2.Addr, err)
	}
	stopService(t, svc)
}

func TestServiceRestartFixedPort(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	addr := freeLoopbackAddr(t)

	st1, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, addr),
	})
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if st1.Addr != addr {
		t.Fatalf("first Addr = %q, want %q", st1.Addr, addr)
	}
	stopService(t, svc)

	st2, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, addr),
	})
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if st2.Addr != addr {
		t.Fatalf("second Addr = %q, want same fixed port %q", st2.Addr, addr)
	}
	stopService(t, svc)
}

func TestServiceRestartNewIdentity(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	start := func(instanceID string) {
		t.Helper()
		_, err := svc.Start(context.Background(), StartOptions{
			Config:      captureConfig(t, "127.0.0.1:0"),
			DesktopMode: true,
			InstanceID:  instanceID,
			Token:       "tok",
		})
		if err != nil {
			t.Fatalf("Start(%q) error = %v", instanceID, err)
		}
		if got := svc.InstanceID(); got != instanceID {
			t.Fatalf("InstanceID() = %q, want %q", got, instanceID)
		}
		stopService(t, svc)
	}

	start("first-instance")
	start("second-instance")
}

func TestServiceLoggerConsumerBaseline(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	dbPath := filepath.Join(t.TempDir(), "moonbridge.db")
	before := logger.ConsumeFuncCount()

	for i := 0; i < 2; i++ {
		addr := freeLoopbackAddr(t)
		if _, err := svc.Start(context.Background(), StartOptions{
			Config: transformConfig(t, addr, dbPath),
		}); err != nil {
			t.Fatalf("Start() iteration %d error = %v", i, err)
		}
		stopService(t, svc)
	}

	if after := logger.ConsumeFuncCount(); after != before {
		t.Fatalf("plugin consumer leaked across restarts: before=%d after=%d", before, after)
	}
}

func TestServiceDesktopModeE2E(t *testing.T) {
	addr := freeLoopbackAddr(t)
	svc := NewService(ServiceOptions{Errors: io.Discard})
	st, err := svc.Start(context.Background(), StartOptions{
		Config:      captureConfig(t, addr),
		DesktopMode: true,
		InstanceID:  "test-instance",
		Token:       "sekret",
		ServerToken: "server-tok",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if st.Status != StatusRunning {
		t.Fatalf("Start() status = %v, want running", st.Status)
	}

	// Without the bearer token the management endpoint is forbidden.
	resp, err := http.Get("http://" + addr + "/api/v1/system/status")
	if err != nil {
		t.Fatalf("GET status (no token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET status (no token) status = %d, want 403", resp.StatusCode)
	}

	// With the token the full desktop contract is served.
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/v1/system/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET status (token): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status (token) status = %d, want 200", resp.StatusCode)
	}
	body := readStatusBody(t, resp)
	if got := body["desktop_mode"]; got != true {
		t.Fatalf("desktop_mode = %v, want true", got)
	}
	if got := body["instance_id"]; got != "test-instance" {
		t.Fatalf("instance_id = %v, want test-instance", got)
	}
	if got := body["api_version"]; got != float64(2) {
		t.Fatalf("api_version = %v, want 2", got)
	}

	// pid must be this process, started_at a valid RFC3339Nano timestamp, and
	// capabilities the base desktop contract strings. Type assertions are safe:
	// a shape mismatch fails the test instead of panicking it.
	pid, ok := body["pid"].(float64)
	if !ok {
		t.Fatalf("pid = %#v, want number", body["pid"])
	}
	if got := int(pid); got != os.Getpid() {
		t.Fatalf("pid = %d, want %d", got, os.Getpid())
	}
	s, ok := body["started_at"].(string)
	if !ok || s == "" {
		t.Fatal("started_at missing or empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		t.Fatalf("started_at %q not a valid timestamp: %v", s, err)
	}
	caps, ok := body["capabilities"].([]any)
	if !ok {
		t.Fatalf("capabilities = %#v, want array", body["capabilities"])
	}
	want := map[string]bool{"config_init": true, "instance_identity": true, "graceful_shutdown": true}
	if len(caps) < len(want) {
		t.Fatalf("capabilities = %v, want at least %d entries", caps, len(want))
	}
	for _, c := range caps {
		name, ok := c.(string)
		if !ok {
			t.Fatalf("capability %#v is not a string", c)
		}
		delete(want, name)
	}
	for name := range want {
		t.Fatalf("capabilities missing %q", name)
	}

	// Graceful shutdown via the management API stops the run with nil error.
	req, err = http.NewRequest(http.MethodPost, "http://"+addr+"/api/v1/system/shutdown", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST shutdown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST shutdown status = %d, want 202", resp.StatusCode)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- svc.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait() after shutdown = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after shutdown")
	}
	if got := svc.Status().Status; got != StatusStopped {
		t.Fatalf("Status() after shutdown = %v, want stopped", got)
	}
}

func readStatusBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode status body: %v", err)
	}
	return body
}

func TestIsLoopbackAddress(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:38440", true},
		{"[::1]:38440", true},
		{"localhost:38440", false},
		{"0.0.0.0:38440", false},
		{"[::]:38440", false},
		{":38440", false},
		{"not-an-addr", false},
	}
	for _, c := range cases {
		if got := IsLoopbackAddress(c.addr); got != c.want {
			t.Errorf("IsLoopbackAddress(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestServiceDesktopModeLoopbackRejected(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	for _, addr := range []string{"0.0.0.0:9999", "localhost:9999", "[::]:9999"} {
		st, err := svc.Start(context.Background(), StartOptions{
			Config:      captureConfig(t, addr),
			DesktopMode: true,
			InstanceID:  "x",
			Token:       "y",
		})
		if !errors.Is(err, ErrDesktopModeRequiresLoopback) {
			t.Fatalf("Start(%q) error = %v, want ErrDesktopModeRequiresLoopback", addr, err)
		}
		if st.Status != StatusStopped {
			t.Fatalf("Start(%q) changed state to %v, want stopped", addr, st.Status)
		}
	}
}

func TestServiceDesktopModeIdentityRequired(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	addr := freeLoopbackAddr(t)

	// Missing InstanceID.
	_, err := svc.Start(context.Background(), StartOptions{
		Config:      captureConfig(t, addr),
		DesktopMode: true,
		Token:       "y",
	})
	if !errors.Is(err, ErrDesktopModeRequiresIdentity) {
		t.Fatalf("missing InstanceID error = %v, want ErrDesktopModeRequiresIdentity", err)
	}
	if got := svc.Status().Status; got != StatusStopped {
		t.Fatalf("state after rejected Start = %v, want stopped", got)
	}

	// Missing Token.
	_, err = svc.Start(context.Background(), StartOptions{
		Config:      captureConfig(t, addr),
		DesktopMode: true,
		InstanceID:  "x",
	})
	if !errors.Is(err, ErrDesktopModeRequiresIdentity) {
		t.Fatalf("missing Token error = %v, want ErrDesktopModeRequiresIdentity", err)
	}
}

func TestServiceDoubleStart(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	if _, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	}); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	_, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Start() error = %v, want ErrAlreadyRunning", err)
	}
	stopService(t, svc)
}

func TestServiceStopMultiple(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	if _, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	stopService(t, svc)

	// A second Stop when nothing is running is a no-op.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Stop(ctx); err != nil {
		t.Fatalf("Stop() after run ended = %v, want nil", err)
	}
}

func TestServiceConcurrentStopIsIdempotent(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	if _, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	results := make(chan error, 2)
	for range 2 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			results <- svc.Stop(ctx)
		}()
	}

	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Stop() error = %v", err)
		}
	}

	if got := svc.Status().Status; got != StatusStopped {
		t.Fatalf("Status() = %v, want stopped", got)
	}
}

func TestServiceBindFailureThenRestart(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	occupied := listener.Addr().String()

	svc := NewService(ServiceOptions{Errors: io.Discard})
	st, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, occupied),
	})
	if err == nil {
		t.Fatal("Start() error = nil, want bind failure")
	}
	if st.Status != StatusFailed {
		t.Fatalf("Start() status = %v, want failed", st.Status)
	}
	if svc.Status().LastError == "" {
		t.Fatal("LastError empty after bind failure")
	}

	// Release the port and restart on a fresh address.
	if err := listener.Close(); err != nil {
		t.Fatalf("close occupied listener: %v", err)
	}
	addr := freeLoopbackAddr(t)
	st, err = svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, addr),
	})
	if err != nil {
		t.Fatalf("restart Start() error = %v", err)
	}
	if st.Status != StatusRunning {
		t.Fatalf("restart Start() status = %v, want running", st.Status)
	}
	if svc.Status().LastError != "" {
		t.Fatalf("LastError not cleared on restart: %q", svc.Status().LastError)
	}
	stopService(t, svc)
}

func TestServiceWaitNaturalExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	svc := NewService(ServiceOptions{Errors: io.Discard})
	if _, err := svc.Start(ctx, StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// The server exits naturally when the parent context is canceled.
	cancel()
	done := make(chan error, 1)
	go func() { done <- svc.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait() after natural exit = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return after natural exit")
	}
	if got := svc.Status().Status; got != StatusStopped {
		t.Fatalf("Status() = %v, want stopped", got)
	}
}

func TestServiceWaitNotRunning(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	if err := svc.Wait(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Wait() before any Start = %v, want ErrNotRunning", err)
	}
}

func TestServiceStartCanceled(t *testing.T) {
	// Pre-canceled context: Start must report ErrStartCanceled and the run
	// must terminate as stopped with no LastError.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc := NewService(ServiceOptions{Errors: io.Discard})
	_, err := svc.Start(ctx, StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	})
	if !errors.Is(err, ErrStartCanceled) {
		t.Fatalf("Start() with canceled ctx = %v, want ErrStartCanceled", err)
	}

	if err := svc.Wait(); err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}
	st := svc.Status()
	if st.Status != StatusStopped {
		t.Fatalf("Status() = %v, want stopped", st.Status)
	}
	if st.LastError != "" {
		t.Fatalf("LastError = %q, want empty", st.LastError)
	}
}

func TestServiceStartCanceledClassification(t *testing.T) {
	// White-box: a run whose ctx is canceled and whose server returned nil
	// (graceful cancel path) must classify the startup as ErrStartCanceled,
	// reported exactly once.
	svc := NewService(ServiceOptions{Errors: io.Discard})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runCtx, runCancel := context.WithCancel(ctx)
	run := &runState{
		id:      1,
		ctx:     runCtx,
		cancel:  runCancel,
		done:    make(chan struct{}),
		started: make(chan startResult, 1),
	}
	svc.mu.Lock()
	svc.current = run
	svc.state = State{Status: StatusStarting}
	svc.mu.Unlock()

	// Simulate app.RunServerWithOptions returning nil on a canceled run ctx.
	svc.finishRun(run, nil)
	state, err := svc.classifyStartup(run, nil)
	run.completeStartup(state, err)

	if !errors.Is(err, ErrStartCanceled) {
		t.Fatalf("classifyStartup() err = %v, want ErrStartCanceled", err)
	}
	select {
	case res := <-run.started:
		if !errors.Is(res.err, ErrStartCanceled) {
			t.Fatalf("startup result err = %v, want ErrStartCanceled", res.err)
		}
		if res.state.Status != StatusStopped {
			t.Fatalf("startup result status = %v, want stopped", res.state.Status)
		}
	default:
		t.Fatal("startup result was not reported")
	}

	// The Once guard must suppress a second report.
	run.completeStartup(state, nil)
	if len(run.started) != 0 {
		t.Fatal("startup result reported more than once")
	}
}

func TestServiceOnListeningGuards(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	run := &runState{id: 1, started: make(chan startResult, 1)}

	// A delayed onListening while stopping must not regress to running.
	svc.mu.Lock()
	svc.current = run
	svc.state = State{Status: StatusStopping}
	svc.mu.Unlock()
	svc.onListening(run, "127.0.0.1:1234")
	if got := svc.Status().Status; got != StatusStopping {
		t.Fatalf("Status() after stopping onListening = %v, want stopping", got)
	}
	if len(run.started) != 0 {
		t.Fatal("startup reported while stopping")
	}

	// Only while starting does onListening transition to running.
	svc.mu.Lock()
	svc.state.Status = StatusStarting
	svc.mu.Unlock()
	svc.onListening(run, "127.0.0.1:1234")
	st := svc.Status()
	if st.Status != StatusRunning {
		t.Fatalf("Status() after starting onListening = %v, want running", st.Status)
	}
	if st.Addr != "127.0.0.1:1234" {
		t.Fatalf("Addr = %q, want 127.0.0.1:1234", st.Addr)
	}
	select {
	case res := <-run.started:
		if res.err != nil || res.state.Status != StatusRunning {
			t.Fatalf("startup result = %v, %v; want running, nil", res.state.Status, res.err)
		}
	default:
		t.Fatal("startup result not reported on bind")
	}

	// A stale callback from a different run must be ignored.
	other := &runState{id: 2}
	svc.onListening(other, "127.0.0.1:9999")
	if got := svc.Status().Addr; got != "127.0.0.1:1234" {
		t.Fatalf("stale run mutated Addr to %q", got)
	}
}

func TestServiceStaleRunGuard(t *testing.T) {
	svc := NewService(ServiceOptions{Errors: io.Discard})
	if _, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	stopService(t, svc)

	// A finishRun from a stale run pointer must not touch the state.
	svc.finishRun(&runState{id: 999, done: make(chan struct{})}, errors.New("stale error"))
	st := svc.Status()
	if st.Status != StatusStopped {
		t.Fatalf("Status() after stale finishRun = %v, want stopped", st.Status)
	}
	if st.LastError != "" {
		t.Fatalf("LastError after stale finishRun = %q, want empty", st.LastError)
	}
}

func TestServicePanicBeforeBind(t *testing.T) {
	// Inject a runner that panics before OnListening. Start must surface the
	// panic through the real recover path in runGoroutine as a failed state
	// and error, and the process must survive to be restarted.
	svc := NewService(ServiceOptions{Errors: io.Discard})
	svc.runServer = func(context.Context, config.Config, io.Writer, app.RunOptions) error {
		panic("boom before bind")
	}
	st, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	})
	if st.Status != StatusFailed {
		t.Fatalf("Start() status = %v, want failed", st.Status)
	}
	if err == nil || !strings.Contains(err.Error(), "gateway run panic") {
		t.Fatalf("Start() error = %v, want panic error", err)
	}
	if svc.Status().LastError == "" {
		t.Fatal("LastError empty after panic")
	}

	// The panic was isolated: restore the real runner and restart.
	svc.runServer = app.RunServerWithOptions
	st, err = svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	})
	if err != nil {
		t.Fatalf("restart Start() error = %v", err)
	}
	if st.Status != StatusRunning {
		t.Fatalf("restart Start() status = %v, want running", st.Status)
	}
	stopService(t, svc)
}

func TestServicePanicAfterOnListening(t *testing.T) {
	// A runner that binds, notifies OnListening, then panics: the bind wins
	// the startup Once, so Start returns running,nil; the panic is reported
	// through Wait() and the run ends failed.
	svc := NewService(ServiceOptions{Errors: io.Discard})
	svc.runServer = func(ctx context.Context, cfg config.Config, _ io.Writer, opts app.RunOptions) error {
		listener, err := net.Listen("tcp", cfg.Addr)
		if err != nil {
			return err
		}
		defer listener.Close()
		if opts.OnListening != nil {
			opts.OnListening(listener.Addr().String())
		}
		panic("boom after bind")
	}
	st, err := svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	})
	if st.Status != StatusRunning {
		t.Fatalf("Start() status = %v, want running", st.Status)
	}
	if err != nil {
		t.Fatalf("Start() error = %v, want nil after bind", err)
	}

	waitErr := svc.Wait()
	if waitErr == nil || !strings.Contains(waitErr.Error(), "gateway run panic") {
		t.Fatalf("Wait() error = %v, want panic error", waitErr)
	}
	if got := svc.Status().Status; got != StatusFailed {
		t.Fatalf("Status() = %v, want failed", got)
	}
	if svc.Status().LastError == "" {
		t.Fatal("LastError empty after post-bind panic")
	}

	// The next run must not inherit the failed run's state.
	svc.runServer = app.RunServerWithOptions
	st, err = svc.Start(context.Background(), StartOptions{
		Config: captureConfig(t, "127.0.0.1:0"),
	})
	if err != nil {
		t.Fatalf("restart Start() error = %v", err)
	}
	if st.Status != StatusRunning {
		t.Fatalf("restart Start() status = %v, want running", st.Status)
	}
	if svc.Status().LastError != "" {
		t.Fatalf("LastError not cleared on restart: %q", svc.Status().LastError)
	}
	stopService(t, svc)
}
