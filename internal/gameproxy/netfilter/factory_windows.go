//go:build windows && amd64 && cgo && game_proxy

package netfilter

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"slices"

	"bork/internal/gameproxy/intercept"
)

// Keep this embed in the cgo GUI build, not the pure-Go helper it embeds.
//
//go:embed helper/bork-driver-helper.exe
var embeddedDriverHelper []byte

type Factory struct {
	materializer artifactMaterializer
	cacheErr     error
	preparer     *driverPreparer
}

func NewFactory() *Factory {
	cacheRoot, err := os.UserCacheDir()
	return &Factory{
		materializer: artifactMaterializer{
			cacheRoot: cacheRoot,
			spec: artifactSpec{
				version:  netFilterSDKVersion,
				filename: netFilterDLLName,
				digest:   netFilterDLLSHA256,
				contents: embeddedNetFilterDLL,
			},
			publish: os.Link,
		},
		cacheErr: err,
		preparer: newDriverPreparer(cacheRoot, embeddedDriverHelper),
	}
}

func (*Factory) Supported() bool { return true }

func (factory *Factory) EnsureAvailable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if factory.cacheErr != nil {
		return fmt.Errorf("locate user cache: %w", factory.cacheErr)
	}
	if _, err := factory.materializer.materialize(ctx); err != nil {
		return err
	}
	return factory.preparer.ensure(ctx)
}

func (factory *Factory) New(ctx context.Context, executablePaths []string) (intercept.Bridge, error) {
	// Driver preparation belongs to EnsureAvailable, before iWAN startup.
	// Bridge construction must not open another elevation prompt.
	if factory.cacheErr != nil {
		return nil, fmt.Errorf("locate user cache: %w", factory.cacheErr)
	}
	dllPath, err := factory.materializer.materialize(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paths := slices.Clone(executablePaths)
	backend, err := newNativeBackend(dllPath, netFilterDriverName)
	if err != nil {
		return nil, fmt.Errorf("construct native backend: %w", err)
	}
	return newBridge(paths, backend)
}
