package app

import (
	"embed"
	"log/slog"
	"path/filepath"

	"bork/internal/config"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

func RunGUI(cfg config.AppConfig, assets embed.FS, logger *slog.Logger) error {
	application := NewApp(cfg, logger)
	return wails.Run(&options.App{
		Title:     "Bork",
		Width:     900,
		Height:    620,
		MinWidth:  800,
		MinHeight: 600,
		Frameless: true,
		Mac:       &mac.Options{},
		Windows: &windows.Options{
			WebviewUserDataPath: filepath.Dir(cfg.FilePath),
		},
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 13, G: 15, B: 18, A: 1},
		OnStartup:        application.startup,
		OnBeforeClose:    application.beforeClose,
		OnShutdown:       application.shutdown,
		Bind:             []interface{}{application},
	})
}
