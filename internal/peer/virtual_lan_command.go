package peer

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

func runVirtualLANCommand(parent context.Context, name string, arguments ...string) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, arguments...).CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w: %s", name, ctx.Err(), output)
	}
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, output)
	}
	return nil
}

func runVirtualLANCleanupCommand(name string, arguments ...string) error {
	return runVirtualLANCommand(context.Background(), name, arguments...)
}
