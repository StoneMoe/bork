//go:build !windows

package config

import "os"

func replaceConfigFile(temporaryPath, targetPath string) error {
	return os.Rename(temporaryPath, targetPath)
}
