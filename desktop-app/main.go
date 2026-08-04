package main

import (
	"embed"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"moonbridge/internal/logger"
)

//go:embed frontend/*
var assets embed.FS

func main() {
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
		Title:      "Moon Bridge Desktop",
		Width:      1100,
		Height:     720,
		MinWidth:   760,
		MinHeight:  520,
		Bind:       []interface{}{app},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
	}); err != nil {
		panic(err)
	}
}
