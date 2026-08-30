package netfilter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var (
	errArtifactNotRegular = errors.New("artifact is not a regular file")
	errArtifactChanged    = errors.New("artifact changed while opening")
)

type artifactSpec struct {
	version  string
	filename string
	digest   string
	contents []byte
}

type artifactMaterializer struct {
	cacheRoot string
	spec      artifactSpec
	publish   func(string, string) error
}

func (materializer artifactMaterializer) targetPath() string {
	return filepath.Join(
		materializer.cacheRoot,
		"bork", "netfilter", materializer.spec.version, materializer.spec.digest,
		materializer.spec.filename,
	)
}

func (materializer artifactMaterializer) materialize(ctx context.Context) (path string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := verifyEmbeddedArtifact(materializer.spec); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	target := materializer.targetPath()
	if err := verifyArtifact(ctx, target, materializer.spec.digest); err == nil {
		return target, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create artifact cache directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	temporaryPath, err := writeTemporaryArtifact(ctx, parent, materializer.spec.contents)
	if err != nil {
		return "", err
	}
	defer func() {
		removeErr := os.Remove(temporaryPath)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temporary artifact: %w", removeErr))
		}
	}()

	if err := verifyArtifact(ctx, temporaryPath, materializer.spec.digest); err != nil {
		return "", fmt.Errorf("verify temporary artifact: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if publishErr := materializer.publish(temporaryPath, target); publishErr != nil {
		winnerErr := verifyArtifact(ctx, target, materializer.spec.digest)
		if winnerErr == nil {
			return target, nil
		}
		if errors.Is(winnerErr, os.ErrNotExist) {
			return "", fmt.Errorf("publish artifact: %w", publishErr)
		}
		return "", errors.Join(fmt.Errorf("publish artifact: %w", publishErr), winnerErr)
	}
	return target, nil
}

func verifyEmbeddedArtifact(spec artifactSpec) error {
	sum := sha256.Sum256(spec.contents)
	actual := hex.EncodeToString(sum[:])
	if actual != spec.digest {
		return &ArtifactIntegrityError{
			Path:  "embedded " + spec.filename,
			Cause: fmt.Errorf("SHA-256 %s, want %s", actual, spec.digest),
		}
	}
	return nil
}

func verifyArtifact(ctx context.Context, path, expectedDigest string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect artifact %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return &ArtifactIntegrityError{Path: path, Cause: errArtifactNotRegular}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open artifact %q: %w", path, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close artifact %q: %w", path, closeErr))
		}
	}()
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened artifact %q: %w", path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return &ArtifactIntegrityError{Path: path, Cause: errArtifactChanged}
	}

	digest := sha256.New()
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if _, hashErr := digest.Write(buffer[:count]); hashErr != nil {
				return fmt.Errorf("hash artifact %q: %w", path, hashErr)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read artifact %q: %w", path, readErr)
		}
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if actual != expectedDigest {
		return &ArtifactIntegrityError{
			Path:  path,
			Cause: fmt.Errorf("SHA-256 %s, want %s", actual, expectedDigest),
		}
	}
	return ctx.Err()
}

func writeTemporaryArtifact(ctx context.Context, parent string, contents []byte) (path string, err error) {
	temporary, err := os.CreateTemp(parent, ".nfapi-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporary != nil {
			if closeErr := temporary.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close temporary artifact: %w", closeErr))
			}
		}
		if err != nil {
			if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove temporary artifact: %w", removeErr))
			}
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", fmt.Errorf("secure temporary artifact: %w", err)
	}
	remaining := contents
	for len(remaining) > 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count, writeErr := temporary.Write(remaining)
		if writeErr != nil {
			return "", fmt.Errorf("write temporary artifact: %w", writeErr)
		}
		if count == 0 {
			return "", fmt.Errorf("write temporary artifact: %w", io.ErrShortWrite)
		}
		remaining = remaining[count:]
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("sync temporary artifact: %w", err)
	}
	if closeErr := temporary.Close(); closeErr != nil {
		temporary = nil
		return "", fmt.Errorf("close temporary artifact: %w", closeErr)
	}
	temporary = nil
	return temporaryPath, nil
}
