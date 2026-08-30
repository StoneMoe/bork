package netfilter

import (
	"context"
	"errors"
	"sync"
	"testing"

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

func (*capturingNativeSink) nativeCallbackSink()                  {}
func (*capturingNativeSink) tcpConnected(nativeTCPConnectedEvent) {}
func (sink *capturingNativeSink) tcpSend(_ intercept.NativeID, payload []byte) {
	sink.mu.Lock()
	sink.payload = payload
	sink.mu.Unlock()
}
func (*capturingNativeSink) tcpClosed(intercept.NativeID)     {}
func (*capturingNativeSink) udpCreated(nativeUDPCreatedEvent) {}
func (*capturingNativeSink) udpSend(nativeUDPSendEvent)       {}
func (*capturingNativeSink) udpClosed(intercept.NativeID)     {}

func (sink *capturingNativeSink) tcpPayload() []byte {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]byte(nil), sink.payload...)
}

type panickingNativeSink struct{}

func (panickingNativeSink) nativeCallbackSink()                  {}
func (panickingNativeSink) tcpConnected(nativeTCPConnectedEvent) {}
func (panickingNativeSink) tcpSend(intercept.NativeID, []byte)   { panic("sink failure") }
func (panickingNativeSink) tcpClosed(intercept.NativeID)         {}
func (panickingNativeSink) udpCreated(nativeUDPCreatedEvent)     {}
func (panickingNativeSink) udpSend(nativeUDPSendEvent)           {}
func (panickingNativeSink) udpClosed(intercept.NativeID)         {}

type startCallbackSink struct {
	closed chan intercept.NativeID
}

func (*startCallbackSink) nativeCallbackSink()                  {}
func (*startCallbackSink) tcpConnected(nativeTCPConnectedEvent) {}
func (*startCallbackSink) tcpSend(intercept.NativeID, []byte)   {}
func (sink *startCallbackSink) tcpClosed(id intercept.NativeID) { sink.closed <- id }
func (*startCallbackSink) udpCreated(nativeUDPCreatedEvent)     {}
func (*startCallbackSink) udpSend(nativeUDPSendEvent)           {}
func (*startCallbackSink) udpClosed(intercept.NativeID)         {}
