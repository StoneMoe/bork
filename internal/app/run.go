package app

import (
	"embed"
	"log/slog"
	"path/filepath"
	"strings"

	"bork/internal/config"

	"github.com/wailsapp/wails/v2"
	wailslogger "github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

func RunGUI(cfg config.AppConfig, assets embed.FS, logger *slog.Logger) error {
	application := NewApp(cfg, logger)
	return wails.Run(&options.App{
		Logger:    privateWailsLogger{wailslogger.NewDefaultLogger()},
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

type privateWailsLogger struct{ wailslogger.Logger }

func (logger privateWailsLogger) Trace(message string) {
	// Wails v2 traces entire RPC results, including stored credentials in snapshots.
	if !strings.HasPrefix(message, "json call result data:") {
		logger.Logger.Trace(message)
	}
}
