//go:build !darwin && !linux && !windows

package main

import (
	"fmt"
	"runtime"
)

func tryAssetLock(string) (func(), bool, error) {
	return nil, false, fmt.Errorf("unsupported build host %s", runtime.GOOS)
}
