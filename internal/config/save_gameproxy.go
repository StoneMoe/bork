//go:build game_proxy

package config

import (
	"errors"
	"fmt"
	"os"
)

// SaveGameProxy preserves network settings edited on disk while the app is open.
func (config AppConfig) SaveGameProxy(gameProxy GameProxyConfig) error {
	if _, err := os.Lstat(config.FilePath); err == nil {
		latest, err := loadAppConfigFile(config.FilePath)
		if err != nil {
			return err
		}
		latest.FilePath = config.FilePath
		config = latest
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect client config: %w", err)
	}
	config.GameProxy = gameProxy
	return config.Save()
}
