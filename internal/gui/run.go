package gui

import (
	"context"
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

type Host struct {
	Assets        embed.FS
	Bindings      []interface{}
	OnStartup     func(context.Context)
	OnBeforeClose func(context.Context) bool
	OnShutdown    func(context.Context)
}

func Run(host Host) error {
	return wails.Run(&options.App{
		Title:     "Bork",
		Width:     900,
		Height:    620,
		MinWidth:  720,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			Assets: host.Assets,
		},
		BackgroundColour: &options.RGBA{R: 13, G: 15, B: 18, A: 1},
		OnStartup:        host.OnStartup,
		OnBeforeClose:    host.OnBeforeClose,
		OnShutdown:       host.OnShutdown,
		Bind:             host.Bindings,
	})
}
