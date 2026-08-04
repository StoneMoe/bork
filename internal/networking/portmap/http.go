package portmap

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/huin/goupnp"
)

const (
	upnpHTTPTimeout        = 5 * time.Second
	upnpMaxResponseBytes   = int64(1 << 20)
	upnpMaxResponseHeaders = int64(64 << 10)
)

var (
	errUPnPRedirect         = errors.New("UPnP HTTP redirects are disabled")
	errUPnPResponseTooLarge = errors.New("UPnP HTTP response exceeds size limit")
	boundedUPnPHTTPClient   = newBoundedUPnPHTTPClient(cloneDefaultHTTPTransport())
)

func init() {
	// goupnp uses this global only for device and service XML fetches. Install it
	// once during package initialization, before any mapper can discover clients.
	goupnp.HTTPClientDefault = boundedUPnPHTTPClient
}

func newBoundedUPnPHTTPClient(base http.RoundTripper) *http.Client {
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{
		Transport: &boundedResponseTransport{
			base:             base,
			maxResponseBytes: upnpMaxResponseBytes,
		},
		Timeout: upnpHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errUPnPRedirect
		},
	}
}

func newUPnPSOAPHTTPClient() http.Client {
	return http.Client{
		Transport:     boundedUPnPHTTPClient.Transport,
		Timeout:       boundedUPnPHTTPClient.Timeout,
		CheckRedirect: boundedUPnPHTTPClient.CheckRedirect,
	}
}

func cloneDefaultHTTPTransport() http.RoundTripper {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := transport.Clone()
		clone.MaxResponseHeaderBytes = upnpMaxResponseHeaders
		clone.ResponseHeaderTimeout = upnpHTTPTimeout
		clone.TLSHandshakeTimeout = upnpHTTPTimeout
		return clone
	}
	return http.DefaultTransport
}

type boundedResponseTransport struct {
	base             http.RoundTripper
	maxResponseBytes int64
}

func (transport *boundedResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > transport.maxResponseBytes {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("%w: declared %d bytes, maximum %d", errUPnPResponseTooLarge, response.ContentLength, transport.maxResponseBytes)
	}
	if response.Body != nil {
		response.Body = http.MaxBytesReader(nil, response.Body, transport.maxResponseBytes)
	}
	return response, nil
}
