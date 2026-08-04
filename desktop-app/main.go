package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed frontend/*
var assets embed.FS

func main() {
	app := &App{}
	if err := wails.Run(&options.App{
		Title:     "Moon Bridge Desktop",
		Width:     1100,
		Height:    720,
		MinWidth:  760,
		MinHeight: 520,
		Bind:      []interface{}{app},
		OnStartup: app.startup,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
	}); err != nil {
		panic(err)
	}
}
