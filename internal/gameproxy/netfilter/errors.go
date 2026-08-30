package netfilter

import (
	"errors"
	"fmt"
)

var (
	ErrUnsupported             = errors.New("netfilter: unsupported")
	ErrArtifactIntegrity       = errors.New("netfilter: artifact integrity violation")
	ErrNilBackend              = errors.New("netfilter: nil native backend")
	ErrNilCallbacks            = errors.New("netfilter: nil callbacks")
	ErrEmptyExecutablePaths    = errors.New("netfilter: empty executable paths")
	ErrInvalidExecutablePath   = errors.New("netfilter: invalid executable path")
	ErrDuplicateExecutablePath = errors.New("netfilter: duplicate executable path")
	ErrInvalidNativeDLLPath    = errors.New("netfilter: invalid native DLL path")
	ErrInvalidNativeDriverName = errors.New("netfilter: invalid native driver name")
	ErrNativeOwnerBusy         = errors.New("netfilter: native SDK already has an owner")
	ErrNativeDLLMismatch       = errors.New("netfilter: native SDK DLL path differs from pinned path")
	ErrInvalidNativeAddress    = errors.New("netfilter: invalid native IPv4 address")
	ErrAlreadyStarted          = errors.New("netfilter: bridge already started")
	ErrNotStarted              = errors.New("netfilter: bridge not started")
	ErrClosed                  = errors.New("netfilter: bridge closed")
)

type ArtifactIntegrityError struct {
	Path  string
	Cause error
}

func (failure *ArtifactIntegrityError) Error() string {
	return fmt.Sprintf("netfilter: artifact %q: %v", failure.Path, failure.Cause)
}

func (failure *ArtifactIntegrityError) Unwrap() error {
	return errors.Join(ErrArtifactIntegrity, failure.Cause)
}

type ExecutablePathError struct {
	Index int
	Path  string
	Cause error
}

func (failure *ExecutablePathError) Error() string {
	return fmt.Sprintf("netfilter: executable path %d %q: %v", failure.Index, failure.Path, failure.Cause)
}

func (failure *ExecutablePathError) Unwrap() error { return failure.Cause }
