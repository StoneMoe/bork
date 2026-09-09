package app

import (
	"fmt"
	"os/exec"
	"runtime"
)

func openDirectory(path string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("explorer.exe", path)
	case "darwin":
		command = exec.Command("open", path)
	default:
		command = exec.Command("xdg-open", path)
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("open log directory: %w", err)
	}
	_ = command.Process.Release()
	return nil
}
