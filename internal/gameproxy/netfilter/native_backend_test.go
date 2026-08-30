package netfilter

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"testing"
)

func TestSDKNativeBackend_enforces_owner_exclusivity_and_reuses_pinned_path(t *testing.T) {
	// Given
	coordinator := &nativeProcessCoordinator{}
	firstSDK := &recordingNativeSDK{}
	busySDK := &recordingNativeSDK{}
	secondSDK := &recordingNativeSDK{}
	config, err := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	if err != nil {
		t.Fatal(err)
	}
	first := newSDKNativeBackend(config, firstSDK, coordinator)
	busy := newSDKNativeBackend(config, busySDK, coordinator)
	rules := validNativeRules(t)

	// When
	if err := first.Start(context.Background(), recordingNativeSink{}, rules); err != nil {
		t.Fatal(err)
	}
	busyErr := busy.Start(context.Background(), recordingNativeSink{}, rules)
	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := newSDKNativeBackend(config, secondSDK, coordinator)
	if err := second.Start(context.Background(), recordingNativeSink{}, rules); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	// Then
	if !errors.Is(busyErr, ErrNativeOwnerBusy) {
		t.Fatalf("concurrent Start() error = %v, want ErrNativeOwnerBusy", busyErr)
	}
	if got := firstSDK.eventsSnapshot(); !reflect.DeepEqual(got, []string{"start", "disable", "drain", "shutdown"}) {
		t.Fatalf("first SDK events = %v", got)
	}
	if got := secondSDK.eventsSnapshot(); !reflect.DeepEqual(got, []string{"start", "disable", "drain", "shutdown"}) {
		t.Fatalf("second SDK events = %v", got)
	}
}

func TestSDKNativeBackend_rejects_different_DLL_after_init_attempt(t *testing.T) {
	// Given
	coordinator := &nativeProcessCoordinator{}
	firstConfig, _ := newNativeConfig(`C:\one\nfapi.dll`, "netfilter2")
	secondConfig, _ := newNativeConfig(`C:\two\nfapi.dll`, "netfilter2")
	first := newSDKNativeBackend(firstConfig, &recordingNativeSDK{}, coordinator)
	secondSDK := &recordingNativeSDK{}
	second := newSDKNativeBackend(secondConfig, secondSDK, coordinator)
	rules := validNativeRules(t)

	// When
	if err := first.Start(context.Background(), recordingNativeSink{}, rules); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	err := second.Start(context.Background(), recordingNativeSink{}, rules)
	if closeErr := second.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	// Then
	if !errors.Is(err, ErrNativeDLLMismatch) {
		t.Fatalf("Start() error = %v, want ErrNativeDLLMismatch", err)
	}
	if got := secondSDK.eventsSnapshot(); len(got) != 0 {
		t.Fatalf("second SDK events = %v, want none", got)
	}
}

func TestSDKNativeBackend_Close_disables_callbacks_and_drains_operation_before_free(t *testing.T) {
	// Given
	sdk := &recordingNativeSDK{
		postStarted: make(chan struct{}), releasePost: make(chan struct{}), disabled: make(chan struct{}),
	}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})
	if err := backend.Start(context.Background(), recordingNativeSink{}, validNativeRules(t)); err != nil {
		t.Fatal(err)
	}
	postDone := make(chan error, 1)
	go func() {
		postDone <- backend.PostTCPReceive(context.Background(), 7, []byte("payload"))
	}()
	<-sdk.postStarted
	closeDone := make(chan error, 1)

	// When
	go func() { closeDone <- backend.Close() }()
	<-sdk.disabled
	if sdk.hasEvent("shutdown") {
		t.Fatal("SDK shutdown occurred before admitted operation drained")
	}
	close(sdk.releasePost)

	// Then
	if err := <-postDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if got := sdk.eventsSnapshot(); !reflect.DeepEqual(got, []string{"start", "post TCP", "disable", "drain", "shutdown"}) {
		t.Fatalf("SDK events = %v", got)
	}
}

func TestSDKNativeBackend_pre_cancelled_post_makes_no_SDK_call(t *testing.T) {
	// Given
	sdk := &recordingNativeSDK{}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})
	if err := backend.Start(context.Background(), recordingNativeSink{}, validNativeRules(t)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// When
	err := backend.PostUDPReceive(ctx, 8, netip.MustParseAddrPort("127.0.0.1:9000"), []byte("payload"))

	// Then
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PostUDPReceive() error = %v, want context.Canceled", err)
	}
	if sdk.hasEvent("post UDP") {
		t.Fatal("pre-cancelled operation reached SDK")
	}
	if closeErr := backend.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestSDKNativeBackend_rules_failure_defers_free_until_Close(t *testing.T) {
	// Given
	sdk := &recordingNativeSDK{startResult: nativeStartResult{
		status: nativeStatusFail, initAttempted: true, initSucceeded: true,
	}}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})

	// When
	startErr := backend.Start(context.Background(), recordingNativeSink{}, validNativeRules(t))
	freeBeforeClose := sdk.shutdownFreeSnapshot()
	closeErr := backend.Close()

	// Then
	var nativeErr *NativeStartError
	if !errors.As(startErr, &nativeErr) || nativeErr.Operation != nativeOperationSetRules {
		t.Fatalf("Start() error = %v, want set-rules NativeStartError", startErr)
	}
	if len(freeBeforeClose) != 0 {
		t.Fatalf("free calls before Close = %v, want none", freeBeforeClose)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if got := sdk.shutdownFreeSnapshot(); !reflect.DeepEqual(got, []bool{true}) {
		t.Fatalf("Shutdown free flags = %v, want [true]", got)
	}
}

func TestSDKNativeBackend_init_failure_does_not_call_free(t *testing.T) {
	// Given
	sdk := &recordingNativeSDK{startResult: nativeStartResult{
		status: nativeStatusIOError, initAttempted: true,
	}}
	config, _ := newNativeConfig(`C:\sdk\nfapi.dll`, "netfilter2")
	backend := newSDKNativeBackend(config, sdk, &nativeProcessCoordinator{})

	// When
	startErr := backend.Start(context.Background(), recordingNativeSink{}, validNativeRules(t))
	closeErr := backend.Close()

	// Then
	var nativeErr *NativeStartError
	if !errors.As(startErr, &nativeErr) || nativeErr.Operation != nativeOperationInit {
		t.Fatalf("Start() error = %v, want init NativeStartError", startErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if got := sdk.shutdownFreeSnapshot(); !reflect.DeepEqual(got, []bool{false}) {
		t.Fatalf("Shutdown free flags = %v, want [false]", got)
	}
}
