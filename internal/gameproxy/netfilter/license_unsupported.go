//go:build game_proxy && (!windows || !amd64)

package netfilter

func License() (string, error) {
	return "", ErrUnsupported
}
