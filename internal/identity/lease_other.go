//go:build !windows && !darwin && !linux

package identity

import "errors"

func acquirePlatformLease(string) (func() error, error) {
	return nil, errors.New("identity lease is unsupported on this platform")
}
