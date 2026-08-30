package netfilter

import (
	"slices"
	"strings"

	"bork/internal/gameproxy/intercept"
)

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
	if sink := backend.callbackSink(); sink != nil {
		sink.udpSend(event)
	}
}

func (backend *sdkNativeBackend) deliverUDPClosed(id intercept.NativeID) {
	if sink := backend.callbackSink(); sink != nil {
		sink.udpClosed(id)
	}
}

func (backend *sdkNativeBackend) callbackSink() nativeCallbackSink {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.sink
}
