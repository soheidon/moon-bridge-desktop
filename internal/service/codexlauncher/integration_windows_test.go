//go:build windows

package codexlauncher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"moonbridge/internal/service/publishrecovery"
)

// These tests exercise the real Windows process adapter (CreateProcess with
// CREATE_NEW_CONSOLE + a kill-on-close job object) against a visible PowerShell
// terminal running a temporary .cmd shim as the "codex" binary. A real console
// window flashes briefly per launch; that is the intended proof that the launch
// path works end to end.
//
// The CTRL_BREAK helper is intentionally NOT exercised here: it re-executes the
// desktop binary, whose helper branch lives in desktop-app/main.go and is not
// present in a test binary. Stop in these tests injects a stub that fails, so
// the graceful path is skipped and the job-termination path is what runs. Real
// CTRL_BREAK delivery is validated in the section 16 manual smoke on the Wails
// process.

// stillActiveExitCode is Windows' STILL_ACTIVE (0x103), not named in x/sys
// v0.44.0.
const stillActiveExitCode = 259

// writeShim creates a .cmd that records its working directory to the marker
// file and then exits, leaving the -NoExit PowerShell window open. The marker
// path travels through MOONBRIDGE_TEST_MARKER, which the launcher merges into
// the child's environment from the test process.
func writeShim(t *testing.T) (shimPath, markerPath string) {
	t.Helper()
	dir := t.TempDir()
	markerPath = filepath.Join(dir, "cwd.txt")
	shimPath = filepath.Join(dir, "codex-shim.cmd")
	content := "@echo off\r\n> \"%MOONBRIDGE_TEST_MARKER%\" echo %CD%\r\n"
	if err := os.WriteFile(shimPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return shimPath, markerPath
}

func shimDiscover(shim string) func(ctx context.Context, timeout time.Duration) (string, error) {
	return func(ctx context.Context, timeout time.Duration) (string, error) {
		return shim, nil
	}
}

// forceStopStub simulates a failed CTRL_BREAK delivery so Stop falls through to
// the force path. See the file header comment.
func forceStopStub(ctx context.Context, pid int) error {
	return errors.New("ctrl-break helper not exercised in integration tests")
}

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActiveExitCode
}

func waitNotAlive(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d still alive after stop", pid)
}

func newIntegrationLauncher(t *testing.T, shim string) *Launcher {
	t.Helper()
	pub := recoverySvcAt(t, t.TempDir(), publishrecovery.Dependencies{})
	return New(Options{
		Discover:            shimDiscover(shim),
		SendCtrlBreak:       forceStopStub,
		GracefulStopTimeout: 2 * time.Second,
		ForceStopTimeout:    5 * time.Second,
		Publisher:           pub,
	})
}

func TestWindowsIntegrationLaunchStopRestartShutdown(t *testing.T) {
	shim, _ := writeShim(t)
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	l := newIntegrationLauncher(t, shim)

	st, err := l.Launch(context.Background(), testLaunchOptions(codexHome))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	if st.Status != StatusRunning || st.PID <= 0 {
		t.Fatalf("expected running with a PID, got %+v", st)
	}
	firstPID := st.PID
	for _, name := range codexHomeFiles {
		if _, err := os.Stat(filepath.Join(codexHome, name)); err != nil {
			t.Fatalf("published file missing after real launch: %s", name)
		}
	}
	if !processAlive(firstPID) {
		t.Fatalf("terminal process %d not alive after launch", firstPID)
	}

	// The injected failing ctrl-break stub forces the job-termination path.
	stop, err := l.Stop(context.Background(), StopReasonShutdown)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if stop.Status != StatusStopped || stop.StopReason != StopReasonTimeoutForce {
		t.Fatalf("unexpected stop state: %+v", stop)
	}
	waitNotAlive(t, firstPID)

	// Restart must produce a fresh PID.
	st2, err := l.Restart(context.Background(), testLaunchOptions(codexHome))
	if err != nil {
		t.Fatalf("Restart failed: %v", err)
	}
	if st2.Status != StatusRunning || st2.PID == 0 || st2.PID == firstPID {
		t.Fatalf("restart must yield a new running PID: %+v", st2)
	}
	if !processAlive(st2.PID) {
		t.Fatalf("restarted terminal %d not alive", st2.PID)
	}

	if _, err := l.Stop(context.Background(), StopReasonShutdown); err != nil {
		t.Fatalf("shutdown stop failed: %v", err)
	}
	waitNotAlive(t, st2.PID)
}

func TestWindowsIntegrationWorkingDirectoryAndShimExecution(t *testing.T) {
	shim, marker := writeShim(t)
	// ASCII spaces + parentheses: the .cmd shim writes %CD% in the console's OEM
	// code page, so non-ASCII path bytes would not round-trip. The Japanese-path
	// case is covered by manual smoke (plan section 16 item 17).
	proj := filepath.Join(t.TempDir(), "my project (dev) v2")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOONBRIDGE_TEST_MARKER", marker)
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	l := newIntegrationLauncher(t, shim)

	opts := testLaunchOptions(codexHome)
	opts.ProjectDirectory = proj
	st, err := l.Launch(context.Background(), opts)
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	// Safety net: a failed assertion must not leave the terminal holding the
	// project directory open (which would block the temp-dir cleanup).
	defer func() {
		cur := l.Status()
		if cur.Status == StatusRunning || cur.Status == StatusStopping {
			_, _ = l.Stop(context.Background(), StopReasonShutdown)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, rerr := os.ReadFile(marker); rerr == nil && len(data) > 0 {
			got := strings.TrimSpace(string(data))
			if !strings.EqualFold(got, filepath.Clean(proj)) {
				t.Fatalf("terminal cwd = %q, want %q", got, filepath.Clean(proj))
			}
			if _, err := l.Stop(context.Background(), StopReasonShutdown); err != nil {
				t.Fatal(err)
			}
			waitNotAlive(t, st.PID)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("shim never wrote its working-directory marker")
}

// TestWindowsIntegrationJobKillOnJobClose validates the Win32 contract the
// launcher relies on for orphan prevention: closing the last job handle with
// KILL_ON_JOB_CLOSE terminates every process in the job. A cmd that would live
// a minute is killed by the job close well before that.
func TestWindowsIntegrationJobKillOnJobClose(t *testing.T) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := setKillOnJobClose(job); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		t.Fatal(err)
	}
	cmdline, err := windows.UTF16PtrFromString(`/D /C ping -n 60 127.0.0.1 > NUL`)
	if err != nil {
		t.Fatal(err)
	}
	si := &windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	pi := &windows.ProcessInformation{}
	if err := windows.CreateProcess(exePtr, cmdline, nil, nil, false,
		uint32(windows.CREATE_SUSPENDED|windows.CREATE_NO_WINDOW), nil, nil, si, pi); err != nil {
		t.Fatal(err)
	}
	defer func() {
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(pi.Process)
	}()
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		t.Fatal(err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		t.Fatal(err)
	}
	pid := int(pi.ProcessId)
	if !processAlive(pid) {
		t.Fatal("child died before job close")
	}
	if err := windows.CloseHandle(job); err != nil {
		t.Fatal(err)
	}
	waitNotAlive(t, pid)
}
