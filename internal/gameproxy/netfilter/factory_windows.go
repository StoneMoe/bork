//go:build windows && amd64 && cgo && netfilter_sdk

package netfilter

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"slices"

	"bork/internal/gameproxy/intercept"
)

const (
	netFilterSDKVersion = "1.7.6.7"
	netFilterDLLName    = "nfapi.dll"
	netFilterDLLSHA256  = "f944b933d948c6ea51b89e790accff07bd9a48f9a9e235abd50a6f40ac7c54b0"
)

//go:embed sdk/nfsdk/wfp/bin/release_c_api/x64/nfapi.dll
var embeddedNetFilterDLL []byte

type Factory struct {
	materializer artifactMaterializer
	cacheErr     error
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
	_, err := factory.materializer.materialize(ctx)
	return err
}

func (factory *Factory) New(ctx context.Context, executablePaths []string) (intercept.Bridge, error) {
	if err := factory.EnsureAvailable(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paths := slices.Clone(executablePaths)
	backend, err := newNativeBackend(factory.materializer.targetPath(), "netfilter2")
	if err != nil {
		return nil, fmt.Errorf("construct native backend: %w", err)
	}
	return newBridge(paths, backend)
}
