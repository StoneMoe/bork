//go:build !windows

package screenshare

func listSources() ([]Source, error) {
	return nil, ErrUnsupported
}
