package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

func (config AppConfig) Save() error {
	if config.FilePath == "" {
		return errors.New("client config path is empty")
	}
	parent := filepath.Dir(config.FilePath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create client config directory: %w", err)
	}
	temporaryPath, err := writeTemporaryAppConfig(config, parent)
	if err != nil {
		return err
	}
	defer os.Remove(temporaryPath)

	info, err := os.Lstat(config.FilePath)
	if errors.Is(err, os.ErrNotExist) {
		if err := publishConfigNoReplace(temporaryPath, config.FilePath); err != nil {
			return fmt.Errorf("publish client config: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect client config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("client config is not a regular file")
	}
	if err := replaceConfigFile(temporaryPath, config.FilePath); err != nil {
		return fmt.Errorf("replace client config: %w", err)
	}
	return nil
}

func writeTemporaryAppConfig(config AppConfig, parent string) (_ string, err error) {
	contents, err := yaml.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("encode client config: %w", err)
	}
	temporary, err := os.CreateTemp(parent, ".config-*")
	if err != nil {
		return "", fmt.Errorf("create temporary client config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporary != nil {
			if closeErr := temporary.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close temporary client config: %w", closeErr))
			}
		}
		if err != nil {
			if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove temporary client config: %w", removeErr))
			}
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", fmt.Errorf("secure temporary client config: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return "", fmt.Errorf("write temporary client config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("sync temporary client config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		temporary = nil
		return "", fmt.Errorf("close temporary client config: %w", err)
	}
	temporary = nil
	return temporaryPath, nil
}
