//go:build windows && wintun_embed

package peer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

var (
	wintunLoadMu sync.Mutex
	wintunModule windows.Handle
)

func prepareWintun(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	elevated, err := processIsElevated()
	if err != nil {
		return fmt.Errorf("check Windows elevation: %w", err)
	}
	if !elevated {
		return errors.New("Windows Virtual LAN setup must run Bork as Administrator; non-elevated Wintun preparation is refused")
	}

	wintunLoadMu.Lock()
	defer wintunLoadMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if wintunModule != 0 {
		return nil
	}
	dllPath, directoryLocks, err := ensureWintun(ctx, embeddedWintunDLL, embeddedWintunDLLSize, embeddedWintunDLLSHA256, embeddedWintunLicense)
	if err != nil {
		return fmt.Errorf("prepare Wintun: %w", err)
	}
	defer closeRuntimeDirectoryLocks(directoryLocks)
	module, err := loadVerifiedWintun(dllPath, embeddedWintunDLLSHA256)
	if err != nil {
		return err
	}
	wintunModule = module
	return nil
}
