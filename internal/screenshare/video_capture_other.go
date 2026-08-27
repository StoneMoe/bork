//go:build !windows || !cgo

package screenshare

type videoSource struct{}

func startVideoSource(string, int, int, int) (*videoSource, VideoInfo, error) {
	return nil, VideoInfo{}, ErrUnsupported
}

func (*videoSource) readFrame() (VideoFrame, error) {
	return VideoFrame{}, ErrUnsupported
}

func (*videoSource) forceKeyFrame() error { return ErrUnsupported }

func (*videoSource) close() error {
	return nil
}
