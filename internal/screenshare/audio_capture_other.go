//go:build !windows || !cgo

package screenshare

type audioSource struct{}

func startAudioSource() (*audioSource, error) {
	return nil, ErrUnsupported
}

func (*audioSource) readFrame() (audioPCMFrame, error) {
	return audioPCMFrame{}, ErrUnsupported
}

func (*audioSource) close() error {
	return nil
}
