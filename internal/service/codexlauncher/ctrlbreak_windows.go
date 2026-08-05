//go:build windows

package codexlauncher

import (
	"os"
	"strconv"

	"golang.org/x/sys/windows"
)

const (
	ctrlCEvent     = 0
	ctrlBreakEvent = 1
)

var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole            = kernel32.NewProc("AttachConsole")
	procFreeConsole              = kernel32.NewProc("FreeConsole")
	procSetConsoleCtrlHandler    = kernel32.NewProc("SetConsoleCtrlHandler")
	procGenerateConsoleCtrlEvent = kernel32.NewProc("GenerateConsoleCtrlEvent")
)

// ctrlHandler swallows CTRL_C / CTRL_BREAK so the helper does not terminate
// when its own GenerateConsoleCtrlEvent reaches its attached console.
func ctrlHandler(ctrlType uint32) uintptr {
	switch ctrlType {
	case ctrlCEvent, ctrlBreakEvent:
		return 1 // TRUE: handled
	}
	return 0
}

// RunCtrlBreakHelper is the desktop binary's helper entrypoint (invoked from
// main.go when MOONBRIDGE_CTRL_BREAK_HELPER=1). It attaches to the child's
// console, registers a handler that swallows the signal it is about to send,
// delivers CTRL_BREAK to the child's process group, then cleans up. It never
// touches the Wails process's console state.
func RunCtrlBreakHelper() int {
	raw := os.Getenv("MOONBRIDGE_CTRL_BREAK_CHILD")
	childPID, err := strconv.Atoi(raw)
	if err != nil || childPID <= 0 {
		return 2
	}
	return runCtrlBreakHelper(uint32(childPID))
}

func runCtrlBreakHelper(childPID uint32) int {
	// FreeConsole is safe even when AttachConsole failed; both run on every
	// path so the helper never leaves a console attached.
	defer procFreeConsole.Call()

	if r, _, _ := procAttachConsole.Call(uintptr(childPID)); r == 0 {
		return 3
	}
	handler := windows.NewCallback(ctrlHandler)
	defer procSetConsoleCtrlHandler.Call(handler, 0)
	if r, _, _ := procSetConsoleCtrlHandler.Call(handler, 1); r == 0 {
		return 4
	}
	if r, _, _ := procGenerateConsoleCtrlEvent.Call(ctrlBreakEvent, uintptr(childPID)); r == 0 {
		return 5
	}
	return 0
}
