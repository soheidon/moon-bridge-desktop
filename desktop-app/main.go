package main

import (
	"embed"
	"errors"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"moonbridge/internal/logger"
	"moonbridge/internal/service/codexlauncher"
)

//go:embed frontend/*
var assets embed.FS

func main() {
	// CTRL_BREAK delivery helper: the launcher re-executes this binary with
	// MOONBRIDGE_CTRL_BREAK_HELPER=1. Branch before any Wails / logger init.
	if os.Getenv("MOONBRIDGE_CTRL_BREAK_HELPER") == "1" {
		os.Exit(codexlauncher.RunCtrlBreakHelper())
	}
	// Exit handoff relay helper: re-execs to relay :38440 → original while a
	// running Codex app-server stays alive, then self-terminates. Branch before
	// any Wails / logger / single-instance init.
	if os.Getenv("MOONBRIDGE_HANDOFF_RELAY") == "1" {
		os.Exit(runHandoffRelay())
	}
	releaseInstance, err := acquireSingleInstance()
	if errors.Is(err, errSingleInstanceAlreadyRunning) {
		fmt.Fprintln(os.Stderr, "Moon Bridge Desktop is already running")
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Moon Bridge Desktop could not acquire its startup lock")
		return
	}
	defer releaseInstance()
	// Initialize the process logger exactly once, before any gateway run starts.
	// Re-running Init (e.g. per gateway restart) would rebuild the consume
	// pipeline and drop registered consumers. Config log.level/log.format are
	// not applied here: the desktop loads config per run, so startup-time values
	// would not reflect it.
	if err := logger.Init(logger.Config{Level: logger.LevelInfo, Format: "text", Output: os.Stderr}); err != nil {
		panic(err)
	}
	app := NewApp(AppOptions{})
	if err := wails.Run(&options.App{
		Title:         "Moon Bridge Desktop",
		Width:         1100,
		Height:        720,
		MinWidth:      760,
		MinHeight:     520,
		Bind:          []interface{}{app},
		OnStartup:     app.startup,
		OnDomReady:    app.domReady,
		OnBeforeClose: app.beforeClose,
		OnShutdown:    app.shutdown,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
	}); err != nil {
		panic(err)
	}
}
