//go:build windows

package codexlauncher

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() {
	platformSendCtrlBreak = sendCtrlBreakWindows
}

type windowsRunner struct{}

func newProcessRunner() processRunner { return &windowsRunner{} }

func isUncPath(p string) bool {
	return strings.HasPrefix(p, `\\`) && !strings.HasPrefix(p, `\\?\`)
}

// Start creates the process suspended, assigns it to a kill-on-close job, and
// only then resumes it, so the child can never run outside the job. The
// executable must be an existing regular file; it is passed as lpApplicationName
// so the launch never depends on PATH or the current directory.
func (r *windowsRunner) Start(ctx context.Context, opts startOptions) (ProcessHandle, error) {
	info, err := os.Stat(opts.Executable)
	if err != nil {
		return nil, &Error{Kind: KindStartFailed, Message: "terminal executable not found"}
	}
	if !info.Mode().IsRegular() {
		return nil, &Error{Kind: KindStartFailed, Message: "terminal path is not a regular file"}
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, &Error{Kind: KindStartFailed, Message: "create job object failed", Details: map[string]any{"win32": err.Error()}}
	}
	if err := setKillOnJobClose(job); err != nil {
		windows.CloseHandle(job)
		return nil, &Error{Kind: KindStartFailed, Message: "configure job object failed", Details: map[string]any{"win32": err.Error()}}
	}

	env, err := buildEnvBlock(opts.Env)
	if err != nil {
		windows.CloseHandle(job)
		return nil, &Error{Kind: KindStartFailed, Message: "build environment block failed"}
	}
	exe, err := windows.UTF16PtrFromString(opts.Executable)
	if err != nil {
		windows.CloseHandle(job)
		return nil, &Error{Kind: KindStartFailed, Message: "encode executable path failed"}
	}
	dir, err := windows.UTF16PtrFromString(opts.WorkingDir)
	if err != nil {
		windows.CloseHandle(job)
		return nil, &Error{Kind: KindStartFailed, Message: "encode working directory failed"}
	}
	cmdline, err := windows.UTF16PtrFromString(opts.CommandLine)
	if err != nil {
		windows.CloseHandle(job)
		return nil, &Error{Kind: KindStartFailed, Message: "encode command line failed"}
	}

	si := &windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	pi := &windows.ProcessInformation{}
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT |
		windows.CREATE_NEW_CONSOLE |
		windows.CREATE_NEW_PROCESS_GROUP |
		windows.CREATE_SUSPENDED)

	if err := windows.CreateProcess(exe, cmdline, nil, nil, false, flags, &env[0], dir, si, pi); err != nil {
		windows.CloseHandle(job)
		return nil, &Error{Kind: KindStartFailed, Message: "create process failed", Details: map[string]any{"win32": err.Error()}}
	}

	proc := &windowsProcess{
		process:  pi.Process,
		thread:   pi.Thread,
		job:      job,
		pid:      pi.ProcessId,
		exitCode: -1,
	}
	// Any failure before resume must terminate the still-suspended child and
	// close every handle without ever resuming it.
	fail := func(err error) (ProcessHandle, error) {
		_ = windows.TerminateProcess(pi.Process, 1)
		_ = proc.Close()
		return nil, err
	}

	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		return fail(&Error{Kind: KindStartFailed, Message: "assign process to job failed", Details: map[string]any{"win32": err.Error()}})
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		return fail(&Error{Kind: KindStartFailed, Message: "resume thread failed", Details: map[string]any{"win32": err.Error()}})
	}
	// The thread handle is not needed after resume.
	windows.CloseHandle(pi.Thread)
	proc.thread = 0
	return proc, nil
}

type windowsProcess struct {
	mu       sync.Mutex
	process  windows.Handle
	thread   windows.Handle
	job      windows.Handle
	pid      uint32
	closed   bool
	exitCode int
}

func (p *windowsProcess) PID() int { return int(p.pid) }

func (p *windowsProcess) Wait(ctx context.Context) error {
	if _, err := windows.WaitForSingleObject(p.process, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.process, &code); err != nil {
		return err
	}
	p.mu.Lock()
	p.exitCode = int(code)
	p.mu.Unlock()
	return nil
}

func (p *windowsProcess) Terminate() error {
	return windows.TerminateJobObject(p.job, 1)
}

func (p *windowsProcess) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.process != 0 {
		windows.CloseHandle(p.process)
		p.process = 0
	}
	if p.thread != 0 {
		windows.CloseHandle(p.thread)
		p.thread = 0
	}
	if p.job != 0 {
		windows.CloseHandle(p.job)
		p.job = 0
	}
	return nil
}

func (p *windowsProcess) ExitCode() (*int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exitCode < 0 {
		return nil, fmt.Errorf("exit code not available")
	}
	ec := p.exitCode
	return &ec, nil
}

func setKillOnJobClose(job windows.Handle) error {
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return err
}

// ctrlBreakHelperTimeout bounds how long Stop waits for the helper process to
// deliver CTRL_BREAK and exit. If it does not, the child is force-stopped.
const ctrlBreakHelperTimeout = 5 * time.Second

// sendCtrlBreakWindows re-executes the desktop binary as a detached helper that
// attaches to the child's console, swallows its own CTRL_BREAK, and forwards
// CTRL_BREAK to the child's process group. The helper never touches the Wails
// console, and the Wails process itself is never re-entered here.
func sendCtrlBreakWindows(ctx context.Context, childPID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	helper, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err := windows.UTF16PtrFromString(helper)
	if err != nil {
		return err
	}
	env := MergeEnv(os.Environ(), map[string]string{
		"MOONBRIDGE_CTRL_BREAK_HELPER": "1",
		"MOONBRIDGE_CTRL_BREAK_CHILD":  strconv.Itoa(childPID),
	})
	envBlock, err := buildEnvBlock(env)
	if err != nil {
		return err
	}
	si := &windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	pi := &windows.ProcessInformation{}
	if err := windows.CreateProcess(exe, nil, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.DETACHED_PROCESS,
		&envBlock[0], nil, si, pi); err != nil {
		return err
	}
	defer func() {
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(pi.Thread)
	}()
	ev, err := windows.WaitForSingleObject(pi.Process, uint32(ctrlBreakHelperTimeout/time.Millisecond))
	if err != nil {
		return err
	}
	if ev != 0 {
		_ = windows.TerminateProcess(pi.Process, 1)
		return fmt.Errorf("ctrl-break helper did not exit in time")
	}
	return nil
}
