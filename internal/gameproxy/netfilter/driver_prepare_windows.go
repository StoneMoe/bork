//go:build windows && amd64 && game_proxy

package netfilter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

type driverPreparer struct {
	mu      sync.Mutex
	pending *driverPreparation
	inspect func() (driverState, error)
	launch  func(context.Context) error
	wait    time.Duration
}

type driverPreparation struct {
	done chan struct{}
	err  error
}

func newDriverPreparer(cacheRoot string, helper []byte) *driverPreparer {
	materializer := artifactMaterializer{
		cacheRoot: cacheRoot,
		spec: artifactSpec{
			version: netFilterSDKVersion, filename: "bork-driver-helper.exe",
			digest: driverDigest(helper), contents: helper,
		},
		publish: os.Link,
	}
	return &driverPreparer{
		inspect: inspectDriver,
		launch: func(ctx context.Context) error {
			return launchDriverHelper(ctx, materializer, executeDriverHelper)
		},
		wait: 30 * time.Second,
	}
}

func (preparer *driverPreparer) ensure(ctx context.Context) error {
	state, err := preparer.query(ctx)
	if err != nil || state == driverRunning {
		return err
	}
	preparer.mu.Lock()
	if preparer.pending != nil {
		preparer.mu.Unlock()
		return errDriverInstallInProgress
	}
	if err := ctx.Err(); err != nil {
		preparer.mu.Unlock()
		return err
	}
	job := &driverPreparation{done: make(chan struct{})}
	preparer.pending = job
	preparer.mu.Unlock()
	// ShellExecuteEx can block in UAC. Cancellation must not discard ownership
	// of that call, its cancel event, or its pinned executable and ancestors.
	go func() {
		job.err = preparer.launch(ctx)
		preparer.mu.Lock()
		preparer.pending = nil
		close(job.done)
		preparer.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-job.done:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if job.err != nil {
		return job.err
	}
	state, err = preparer.query(ctx)
	if err != nil {
		return err
	}
	if state != driverRunning {
		return errors.New("netfilter: helper exited successfully without a running driver")
	}
	return ctx.Err()
}

func (preparer *driverPreparer) query(ctx context.Context) (driverState, error) {
	ctx, cancel := context.WithTimeout(ctx, preparer.wait)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return driverMissing, err
		}
		state, err := preparer.inspect()
		if err != nil {
			return driverMissing, err
		}
		if err := ctx.Err(); err != nil {
			return driverMissing, err
		}
		switch state {
		case driverRunning, driverMissing, driverStopped:
			return state, nil
		case driverPending:
		default:
			return driverMissing, fmt.Errorf("%w: unknown driver state %d", errDriverConflict, state)
		}
		select {
		case <-ctx.Done():
			return driverMissing, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type pinnedDriverHelper struct {
	path  string
	files []*os.File
}

func (helper *pinnedDriverHelper) close() {
	for i := len(helper.files) - 1; i >= 0; i-- {
		helper.files[i].Close()
	}
}

func pinDriverHelper(ctx context.Context, materializer artifactMaterializer) (_ *pinnedDriverHelper, err error) {
	target := materializer.targetPath()
	volume := filepath.VolumeName(target)
	if len(materializer.spec.contents) == 0 || len(volume) != 2 || volume[1] != ':' ||
		!strings.ContainsAny(volume[:1], "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz") ||
		!filepath.IsAbs(materializer.cacheRoot) || filepath.Clean(materializer.cacheRoot) != materializer.cacheRoot ||
		!filepath.IsAbs(target) || filepath.Clean(target) != target {
		return nil, fmt.Errorf("%w: unsafe helper cache path or empty payload", errDriverConflict)
	}
	root := volume + `\`
	if windows.GetDriveType(windows.StringToUTF16Ptr(root)) != windows.DRIVE_FIXED {
		return nil, fmt.Errorf("%w: helper cache must be on a local fixed volume", errDriverConflict)
	}
	components := strings.Split(strings.TrimPrefix(target, root), `\`)
	for _, component := range components {
		if component == "" || strings.TrimRight(component, ". ") != component || strings.ContainsAny(component, `/:*?"<>|`+"\x00") {
			return nil, fmt.Errorf("%w: unsafe helper cache component", errDriverConflict)
		}
	}
	// Cache publication is unprivileged and may race other cache users. It must
	// precede pinning: Windows hard-link publication needs write sharing on the
	// parent. Only the subsequent pinned path and digest authorize elevation.
	if _, err := materializer.materialize(ctx); err != nil {
		return nil, err
	}
	helper := &pinnedDriverHelper{path: target}
	defer func() {
		if err != nil {
			helper.close()
		}
	}()
	path := root
	// Pin before descending. No security-owner check:
	// this is the user's cache, not the protected driver installation.
	for i := 0; i < len(components); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, openErr := pinDriverObject(path)
		if openErr != nil {
			return nil, openErr
		}
		helper.files = append(helper.files, file)
		if err := verifyHelperObject(file, path, true); err != nil {
			return nil, err
		}
		path = filepath.Join(path, components[i])
	}
	file, err := pinDriverObject(target)
	if err != nil {
		return nil, err
	}
	helper.files = append(helper.files, file)
	if err := verifyHelperObject(file, target, false); err != nil {
		return nil, err
	}
	if err := verifyDriverContents(file, int64(len(materializer.spec.contents)), materializer.spec.digest); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return helper, nil
}

func verifyHelperObject(file *os.File, path string, directory bool) error {
	handle := windows.Handle(file.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory ||
		(!directory && info.NumberOfLinks != 1) {
		return fmt.Errorf("%w: helper cache contains a reparse point, wrong object type, or hard link", errDriverConflict)
	}
	buffer := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return err
	}
	if n >= uint32(len(buffer)) || !strings.EqualFold(windows.UTF16ToString(buffer[:n]), `\\?\`+path) {
		return fmt.Errorf("%w: helper cache resolved to a different path", errDriverConflict)
	}
	return nil
}
