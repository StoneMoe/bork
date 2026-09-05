package netfilter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"bork/internal/gameproxy/intercept"
)

func TestSDKNativeBackend_deliver_callbacks_copy_payload_before_sink(t *testing.T) {
	// Given
	sdk := &recordingNativeSDK{}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})
	sink := &capturingNativeSink{}
	if err := backend.Start(context.Background(), sink, validNativeRules(t)); err != nil {
		t.Fatal(err)
	}
	payload := []byte("original")

	// When
	backend.deliverTCPSend(11, payload)
	payload[0] = 'X'

	// Then
	if got := string(sink.tcpPayload()); got != "original" {
		t.Fatalf("sink payload = %q, want original", got)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCallbackError_names_process_path_failure(t *testing.T) {
	failure := &NativeCallbackError{
		Event: nativeEventTCPConnected, ID: 1, Reason: nativeReasonProcessPath,
		Status: nativeStatusSuccess, CleanupStatus: nativeStatusSuccess,
	}
	if got := failure.Error(); !strings.Contains(got, "reason process path (5)") {
		t.Fatalf("Error() = %q", got)
	}
}

func TestNativeCallbackError_names_post_receive_failure(t *testing.T) {
	failure := &NativeCallbackError{
		Event: nativeEventTCPReceive, ID: 2, Reason: nativeReasonPostReceive,
		Status: nativeStatusFail, CleanupStatus: nativeStatusSuccess,
	}
	if got := failure.Error(); !strings.Contains(got, "reason post receive (7)") {
		t.Fatalf("Error() = %q", got)
	}
}

func TestSDKNativeBackend_delivers_endpoint_error_without_stopping_Wait(t *testing.T) {
	sdk := &recordingNativeSDK{}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})
	sink := &endpointErrorNativeSink{failures: make(chan *NativeCallbackError, 1)}
	if err := backend.Start(context.Background(), sink, validNativeRules(t)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waitResult := make(chan error, 1)
	go func() { waitResult <- backend.Wait(ctx) }()
	failure := &NativeCallbackError{
		Event: nativeEventUDPCreated, ID: 85, Reason: nativeReasonIPv6,
		Status: nativeStatusFail, CleanupStatus: nativeStatusSuccess,
	}

	backend.deliverEndpointError(failure)

	if got := <-sink.failures; got != failure {
		t.Fatalf("delivered failure = %v, want %v", got, failure)
	}
	select {
	case err := <-waitResult:
		t.Fatalf("Wait stopped after endpoint error: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := <-waitResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context canceled", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSDKNativeBackend_bypass_failure_stops_Wait(t *testing.T) {
	sdk := &recordingNativeSDK{}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})
	if err := backend.Start(context.Background(), recordingNativeSink{}, validNativeRules(t)); err != nil {
		t.Fatal(err)
	}
	failure := &NativeCallbackError{
		Event: nativeEventUDPSend, ID: 84, Reason: nativeReasonSelfInterception,
		Status: nativeStatusFail, CleanupStatus: nativeStatusFail,
	}
	backend.deliverEndpointError(failure)
	if waitErr := backend.Wait(context.Background()); waitErr != failure {
		t.Fatalf("Wait() error = %v, want bypass failure", waitErr)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSDKNativeBackend_backend_status_stops_Wait(t *testing.T) {
	sdk := &recordingNativeSDK{postTCPStatus: nativeStatusIOError}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})
	if err := backend.Start(context.Background(), recordingNativeSink{}, validNativeRules(t)); err != nil {
		t.Fatal(err)
	}
	operationErr := backend.PostTCPReceive(context.Background(), 9, []byte("payload"))
	var statusErr *NativeStatusError
	if !errors.As(operationErr, &statusErr) || statusErr.Status != nativeStatusIOError {
		t.Fatalf("PostTCPReceive() error = %v, want IO status", operationErr)
	}
	if waitErr := backend.Wait(context.Background()); waitErr != operationErr {
		t.Fatalf("Wait() error = %v, want operation error %v", waitErr, operationErr)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSDKNativeBackend_backend_callback_status_stops_Wait_after_delivery(t *testing.T) {
	sdk := &recordingNativeSDK{}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})
	sink := &endpointErrorNativeSink{failures: make(chan *NativeCallbackError, 1)}
	if err := backend.Start(context.Background(), sink, validNativeRules(t)); err != nil {
		t.Fatal(err)
	}
	failure := &NativeCallbackError{
		Event: nativeEventUDPReceive, ID: 86, Reason: nativeReasonPostReceive,
		Status: nativeStatusIOError, CleanupStatus: nativeStatusIOError,
	}
	backend.deliverEndpointError(failure)
	if delivered := <-sink.failures; delivered != failure {
		t.Fatalf("delivered failure = %v, want %v", delivered, failure)
	}
	if waitErr := backend.Wait(context.Background()); waitErr != failure {
		t.Fatalf("Wait() error = %v, want callback failure", waitErr)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchNativeCallback_converts_sink_panic_to_first_Wait_error(t *testing.T) {
	// Given
	sdk := &recordingNativeSDK{}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})
	if err := backend.Start(context.Background(), panickingNativeSink{}, validNativeRules(t)); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	token := backend.token
	backend.mu.Unlock()

	// When
	dispatchNativeCallback(token, nativeEventTCPSend, func(owner *sdkNativeBackend) {
		owner.deliverTCPSend(12, []byte("payload"))
	})
	backend.reportFatal(errors.New("later failure"))
	err := backend.Wait(context.Background())

	// Then
	var callbackErr *NativeCallbackError
	if !errors.As(err, &callbackErr) || callbackErr.Event != nativeEventTCPSend {
		t.Fatalf("Wait() error = %v, want TCP send NativeCallbackError", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSDKNativeBackend_accepts_callback_during_Start(t *testing.T) {
	// Given
	sink := &startCallbackSink{closed: make(chan intercept.NativeID, 1)}
	sdk := &recordingNativeSDK{}
	sdk.startHook = func(token uint64) {
		dispatchNativeCallback(token, nativeEventTCPClosed, func(owner *sdkNativeBackend) {
			owner.deliverTCPClosed(13)
		})
	}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})

	// When
	err := backend.Start(context.Background(), sink, validNativeRules(t))

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if id := <-sink.closed; id != 13 {
		t.Fatalf("callback ID = %d, want 13", id)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSDKNativeBackend_does_not_hold_backend_lock_across_SDK_call(t *testing.T) {
	// Given
	sdk := &recordingNativeSDK{}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})
	sdk.postHook = func() {
		if err := backend.CloseTCP(14); err != nil {
			t.Errorf("reentrant CloseTCP: %v", err)
		}
	}
	if err := backend.Start(context.Background(), recordingNativeSink{}, validNativeRules(t)); err != nil {
		t.Fatal(err)
	}

	// When
	err := backend.PostTCPReceive(context.Background(), 14, []byte("payload"))

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

type capturingNativeSink struct {
	mu      sync.Mutex
	payload []byte
}

type endpointErrorNativeSink struct {
	recordingNativeSink
	failures chan *NativeCallbackError
}

func (sink *endpointErrorNativeSink) endpointError(failure *NativeCallbackError) {
	sink.failures <- failure
}

func (*capturingNativeSink) nativeCallbackSink()                                   {}
func (*capturingNativeSink) tcpConnectRequest(nativeTCPConnectRequestEvent) uint16 { return 0 }
func (*capturingNativeSink) tcpConnected(nativeTCPConnectedEvent)                  {}
func (sink *capturingNativeSink) tcpSend(_ intercept.NativeID, payload []byte) {
	sink.mu.Lock()
	sink.payload = payload
	sink.mu.Unlock()
}
func (*capturingNativeSink) tcpClosed(intercept.NativeID)       {}
func (*capturingNativeSink) udpCreated(nativeUDPCreatedEvent)   {}
func (*capturingNativeSink) udpSend(nativeUDPSendEvent)         {}
func (*capturingNativeSink) udpClosed(intercept.NativeID)       {}
func (*capturingNativeSink) endpointError(*NativeCallbackError) {}

func (sink *capturingNativeSink) tcpPayload() []byte {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]byte(nil), sink.payload...)
}

type panickingNativeSink struct{}

func (panickingNativeSink) nativeCallbackSink()                                   {}
func (panickingNativeSink) tcpConnectRequest(nativeTCPConnectRequestEvent) uint16 { return 0 }
func (panickingNativeSink) tcpConnected(nativeTCPConnectedEvent)                  {}
func (panickingNativeSink) tcpSend(intercept.NativeID, []byte)                    { panic("sink failure") }
func (panickingNativeSink) tcpClosed(intercept.NativeID)                          {}
func (panickingNativeSink) udpCreated(nativeUDPCreatedEvent)                      {}
func (panickingNativeSink) udpSend(nativeUDPSendEvent)                            {}
func (panickingNativeSink) udpClosed(intercept.NativeID)                          {}
func (panickingNativeSink) endpointError(*NativeCallbackError)                    {}

type startCallbackSink struct {
	closed chan intercept.NativeID
}

func (*startCallbackSink) nativeCallbackSink()                                   {}
func (*startCallbackSink) tcpConnectRequest(nativeTCPConnectRequestEvent) uint16 { return 0 }
func (*startCallbackSink) tcpConnected(nativeTCPConnectedEvent)                  {}
func (*startCallbackSink) tcpSend(intercept.NativeID, []byte)                    {}
func (sink *startCallbackSink) tcpClosed(id intercept.NativeID)                  { sink.closed <- id }
func (*startCallbackSink) udpCreated(nativeUDPCreatedEvent)                      {}
func (*startCallbackSink) udpSend(nativeUDPSendEvent)                            {}
func (*startCallbackSink) udpClosed(intercept.NativeID)                          {}
func (*startCallbackSink) endpointError(*NativeCallbackError)                    {}
