//go:build windows && amd64 && game_proxy

package netfilter

// License returns the original embedded RTF without preparing or loading the driver.
func License() (string, error) {
	return embeddedNetFilterLicense, nil
}
