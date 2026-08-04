package portmap

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/soap"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (body *trackingReadCloser) Close() error {
	body.closed = true
	return nil
}

func TestBoundedTransportRejectsDeclaredOversizeBody(t *testing.T) {
	body := &trackingReadCloser{Reader: bytes.NewReader([]byte("ignored"))}
	transport := &boundedResponseTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          body,
				ContentLength: 9,
			}, nil
		}),
		maxResponseBytes: 8,
	}
	request, err := http.NewRequest(http.MethodGet, "http://router.test/device.xml", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := transport.RoundTrip(request)
	if response != nil || !errors.Is(err, errUPnPResponseTooLarge) {
		t.Fatalf("RoundTrip response, error = %v, %v", response, err)
	}
	if !body.closed {
		t.Fatal("oversize response body was not closed")
	}
}

func TestBoundedTransportHardLimitsUnknownBody(t *testing.T) {
	body := &trackingReadCloser{Reader: bytes.NewReader([]byte("123456789"))}
	transport := &boundedResponseTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          body,
				ContentLength: -1,
			}, nil
		}),
		maxResponseBytes: 8,
	}
	request, err := http.NewRequest(http.MethodGet, "http://router.test/device.xml", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(response.Body)
	var maxBytesError *http.MaxBytesError
	if !errors.As(err, &maxBytesError) || maxBytesError.Limit != 8 {
		t.Fatalf("ReadAll error = %v", err)
	}
	if string(read) != "12345678" {
		t.Fatalf("read body = %q", read)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !body.closed {
		t.Fatal("wrapped response body did not close its source")
	}
}

func TestBoundedClientRejectsRedirectsAndHasTimeout(t *testing.T) {
	calls := 0
	client := newBoundedUPnPHTTPClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"http://other.test/device.xml"}},
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Request:    request,
		}, nil
	}))
	if client.Timeout != upnpHTTPTimeout {
		t.Fatalf("client timeout = %s, want %s", client.Timeout, upnpHTTPTimeout)
	}
	request, err := http.NewRequest(http.MethodGet, "http://router.test/device.xml", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if !errors.Is(err, errUPnPRedirect) {
		t.Fatalf("redirect error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("transport calls = %d, want 1", calls)
	}
}

func TestBoundedClientInstalledForGoupnpFetches(t *testing.T) {
	if goupnp.HTTPClientDefault != boundedUPnPHTTPClient {
		t.Fatal("goupnp HTTP client is not the package-scoped bounded client")
	}
	soapClient := newUPnPSOAPHTTPClient()
	if soapClient.Transport != boundedUPnPHTTPClient.Transport || soapClient.Timeout != upnpHTTPTimeout {
		t.Fatal("SOAP client does not share bounded transport and timeout")
	}
	service := &goupnp.ServiceClient{SOAPClient: &soap.SOAPClient{}}
	newIGDGateway(service, nil, providerWANIP2)
	if service.SOAPClient.HTTPClient.Transport != boundedUPnPHTTPClient.Transport || service.SOAPClient.HTTPClient.Timeout != upnpHTTPTimeout {
		t.Fatal("gateway wrapper did not install the bounded SOAP client")
	}
}
