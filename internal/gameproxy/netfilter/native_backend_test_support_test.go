package netfilter

import (
	"net/netip"
	"sync"
	"testing"

	"bork/internal/gameproxy/intercept"
)

func validNativeRules(t *testing.T) []nativeRule {
	t.Helper()
	rules, err := exactRules([]string{`C:\Games\game.exe`})
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

type recordingNativeSink struct{}

func (recordingNativeSink) nativeCallbackSink()                  {}
func (recordingNativeSink) tcpConnected(nativeTCPConnectedEvent) {}
func (recordingNativeSink) tcpSend(intercept.NativeID, []byte)   {}
func (recordingNativeSink) tcpClosed(intercept.NativeID)         {}
func (recordingNativeSink) udpCreated(nativeUDPCreatedEvent)     {}
func (recordingNativeSink) udpSend(nativeUDPSendEvent)           {}
func (recordingNativeSink) udpClosed(intercept.NativeID)         {}

type recordingNativeSDK struct {
	mu           sync.Mutex
	events       []string
	postStarted  chan struct{}
	releasePost  chan struct{}
	disabled     chan struct{}
	startResult  nativeStartResult
	shutdownFree []bool
	startHook    func(uint64)
	postHook     func()
}

func (sdk *recordingNativeSDK) Start(token uint64, _ nativeConfig, _ []nativeRule) nativeStartResult {
	sdk.record("start")
	if sdk.startHook != nil {
		sdk.startHook(token)
	}
	if sdk.startResult.status != nativeStatusSuccess || sdk.startResult.initAttempted || sdk.startResult.initSucceeded {
		return sdk.startResult
	}
	return nativeStartResult{status: nativeStatusSuccess, initAttempted: true, initSucceeded: true}
}

func (sdk *recordingNativeSDK) DisableCallbacks(uint64) {
	sdk.record("disable")
	if sdk.disabled != nil {
		close(sdk.disabled)
	}
}

func (sdk *recordingNativeSDK) DrainCallbacks(uint64) { sdk.record("drain") }
func (sdk *recordingNativeSDK) Shutdown(_ uint64, callFree bool) {
	sdk.mu.Lock()
	sdk.events = append(sdk.events, "shutdown")
	sdk.shutdownFree = append(sdk.shutdownFree, callFree)
	sdk.mu.Unlock()
}

func (sdk *recordingNativeSDK) PostTCPReceive(uint64, intercept.NativeID, []byte) nativeStatus {
	sdk.record("post TCP")
	if sdk.postHook != nil {
		sdk.postHook()
	}
	if sdk.postStarted != nil {
		close(sdk.postStarted)
		<-sdk.releasePost
	}
	return nativeStatusSuccess
}

func (sdk *recordingNativeSDK) CloseTCP(uint64, intercept.NativeID) nativeStatus {
	sdk.record("close TCP")
	return nativeStatusSuccess
}

func (sdk *recordingNativeSDK) PostUDPReceive(uint64, intercept.NativeID, netip.AddrPort, []byte) nativeStatus {
	sdk.record("post UDP")
	return nativeStatusSuccess
}

func (sdk *recordingNativeSDK) SuspendUDP(uint64, intercept.NativeID) nativeStatus {
	sdk.record("suspend UDP")
	return nativeStatusSuccess
}

func (sdk *recordingNativeSDK) record(event string) {
	sdk.mu.Lock()
	sdk.events = append(sdk.events, event)
	sdk.mu.Unlock()
}

func (sdk *recordingNativeSDK) eventsSnapshot() []string {
	sdk.mu.Lock()
	defer sdk.mu.Unlock()
	return append([]string(nil), sdk.events...)
}

func (sdk *recordingNativeSDK) hasEvent(want string) bool {
	for _, event := range sdk.eventsSnapshot() {
		if event == want {
			return true
		}
	}
	return false
}

func (sdk *recordingNativeSDK) shutdownFreeSnapshot() []bool {
	sdk.mu.Lock()
	defer sdk.mu.Unlock()
	return append([]bool(nil), sdk.shutdownFree...)
}
