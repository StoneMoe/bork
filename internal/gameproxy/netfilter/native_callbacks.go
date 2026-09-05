//go:build game_proxy

package netfilter

import (
	"slices"
	"strings"

	"bork/internal/gameproxy/intercept"
)

func (backend *sdkNativeBackend) deliverTCPConnectRequest(event nativeTCPConnectRequestEvent) uint16 {
	event.ExecutablePath = strings.Clone(event.ExecutablePath)
	if sink := backend.callbackSink(); sink != nil {
		return sink.tcpConnectRequest(event)
	}
	return 0
}

func (backend *sdkNativeBackend) deliverTCPConnected(event nativeTCPConnectedEvent) {
	event.ExecutablePath = strings.Clone(event.ExecutablePath)
	if sink := backend.callbackSink(); sink != nil {
		sink.tcpConnected(event)
	}
}

func (backend *sdkNativeBackend) deliverTCPSend(id intercept.NativeID, payload []byte) {
	if sink := backend.callbackSink(); sink != nil {
		sink.tcpSend(id, slices.Clone(payload))
	}
}

func (backend *sdkNativeBackend) deliverTCPClosed(id intercept.NativeID) {
	if sink := backend.callbackSink(); sink != nil {
		sink.tcpClosed(id)
	}
}

func (backend *sdkNativeBackend) deliverUDPCreated(event nativeUDPCreatedEvent) {
	event.ExecutablePath = strings.Clone(event.ExecutablePath)
	if sink := backend.callbackSink(); sink != nil {
		sink.udpCreated(event)
	}
}

func (backend *sdkNativeBackend) deliverUDPSend(event nativeUDPSendEvent) {
	event.Payload = slices.Clone(event.Payload)
	event.Options.data = slices.Clone(event.Options.data)
	if sink := backend.callbackSink(); sink != nil {
		sink.udpSend(event)
	}
}

func (backend *sdkNativeBackend) deliverUDPClosed(id intercept.NativeID) {
	if sink := backend.callbackSink(); sink != nil {
		sink.udpClosed(id)
	}
}

func (backend *sdkNativeBackend) deliverEndpointError(failure *NativeCallbackError) {
	if sink := backend.callbackSink(); sink != nil {
		sink.endpointError(failure)
	}
	bypassFailure := failure != nil &&
		(failure.Reason == nativeReasonIPv6 || failure.Reason == nativeReasonSelfInterception ||
			failure.Reason == nativeReasonLocalRoute) &&
		failure.CleanupStatus != nativeStatusSuccess
	if failure != nil && (bypassFailure || isBackendFatalStatus(failure.Status) || isBackendFatalStatus(failure.CleanupStatus)) {
		backend.reportFatal(failure)
	}
}

func (backend *sdkNativeBackend) callbackSink() nativeCallbackSink {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.sink
}
