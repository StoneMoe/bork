package netfilter

import (
	"errors"
	"net/netip"
	"strings"

	"bork/internal/gameproxy/intercept"
)

func (bridge *Bridge) udpCreated(event nativeUDPCreatedEvent) {
	bridge.mu.Lock()
	if bridge.state != bridgeStarting && bridge.state != bridgeStarted {
		bridge.mu.Unlock()
		bridge.recordCallbackError(bridge.backend.SuspendUDP(event.ID))
		return
	}
	bridge.callbackWG.Add(1)
	defer bridge.callbackWG.Done()
	endpoint := bridge.udpEndpoints[event.ID]
	_, pending := bridge.udpSockets[event.ID]
	if endpoint == nil && !pending && validUDPCreatedEvent(event) {
		event.ExecutablePath = strings.Clone(event.ExecutablePath)
		bridge.udpSockets[event.ID] = event
		bridge.mu.Unlock()
		return
	}
	if pending {
		delete(bridge.udpSockets, event.ID)
	}
	bridge.finishUDPAdmissionLocked(event.ID, bridge.udpAdmissions[event.ID])
	bridge.mu.Unlock()

	failure := &intercept.FlowError{
		NativeID: event.ID, Operation: "create native UDP", Cause: intercept.ErrDuplicateFlow,
	}
	if endpoint != nil {
		bridge.recordCallbackError(endpoint.Reset(failure))
		return
	}
	bridge.recordCallbackError(bridge.backend.SuspendUDP(event.ID))
}

func (bridge *Bridge) udpSend(event nativeUDPSendEvent) {
	bridge.mu.Lock()
	if bridge.state != bridgeStarting && bridge.state != bridgeStarted {
		bridge.mu.Unlock()
		bridge.recordCallbackError(bridge.backend.SuspendUDP(event.ID))
		return
	}
	bridge.callbackWG.Add(1)
	defer bridge.callbackWG.Done()
	callbacks := bridge.callbacks
	resolvedAdmission := false
	for {
		endpoint := bridge.udpEndpoints[event.ID]
		if endpoint != nil {
			bridge.mu.Unlock()
			if endpoint.metadata.OriginalLocal != event.Local || !validUDPSendEvent(event) {
				bridge.rejectUDPEndpoint(endpoint, event.ID, intercept.ErrInvalidFlow)
				return
			}
			bridge.enqueueUDP(endpoint, event.Remote, event.Payload)
			return
		}
		socket, exists := bridge.udpSockets[event.ID]
		if !validUDPSendEvent(event) {
			delete(bridge.udpSockets, event.ID)
			bridge.finishUDPAdmissionLocked(event.ID, bridge.udpAdmissions[event.ID])
			bridge.mu.Unlock()
			bridge.recordCallbackError(bridge.backend.SuspendUDP(event.ID))
			return
		}
		if !exists {
			bridge.mu.Unlock()
			if !resolvedAdmission {
				bridge.recordCallbackError(bridge.backend.SuspendUDP(event.ID))
			}
			return
		}
		if admission := bridge.udpAdmissions[event.ID]; admission != nil {
			done := admission.done
			bridge.mu.Unlock()
			<-done
			resolvedAdmission = true
			bridge.mu.Lock()
			if bridge.state != bridgeStarting && bridge.state != bridgeStarted {
				bridge.mu.Unlock()
				return
			}
			continue
		}

		admission := &udpAdmission{done: make(chan struct{}), socket: socket, send: event}
		bridge.udpAdmissions[event.ID] = admission
		bridge.mu.Unlock()
		state := callbacks.GenerationState()
		bridge.finishUDPAdmission(admission, state)
		return
	}
}

func (bridge *Bridge) udpClosed(id intercept.NativeID) {
	bridge.mu.Lock()
	endpoint := bridge.udpEndpoints[id]
	delete(bridge.udpEndpoints, id)
	delete(bridge.udpSockets, id)
	bridge.finishUDPAdmissionLocked(id, bridge.udpAdmissions[id])
	bridge.mu.Unlock()
	if endpoint != nil {
		endpoint.nativeClosed()
	}
}

func (bridge *Bridge) enqueueUDP(endpoint *udpEndpoint, remote netip.AddrPort, payload []byte) {
	err := endpoint.enqueue(remote, payload)
	if errors.Is(err, intercept.ErrQueueFull) {
		failure := &intercept.FlowError{
			NativeID: endpoint.metadata.NativeID, Operation: "queue native UDP send", Cause: err,
		}
		bridge.recordCallbackError(endpoint.Reset(failure))
	}
}

func (bridge *Bridge) rejectUDPEndpoint(endpoint *udpEndpoint, id intercept.NativeID, cause error) {
	if endpoint != nil {
		failure := &intercept.FlowError{NativeID: id, Operation: "validate native UDP send", Cause: cause}
		bridge.recordCallbackError(endpoint.Reset(failure))
		return
	}
	bridge.recordCallbackError(bridge.backend.SuspendUDP(id))
}

func (bridge *Bridge) detachNativeOperationsLocked() ([]*tcpFlow, []*udpEndpoint, []intercept.NativeID) {
	flows := make([]*tcpFlow, 0, len(bridge.flows))
	for id, flow := range bridge.flows {
		flows = append(flows, flow)
		delete(bridge.flows, id)
	}
	endpoints := make([]*udpEndpoint, 0, len(bridge.udpEndpoints))
	for id, endpoint := range bridge.udpEndpoints {
		endpoints = append(endpoints, endpoint)
		delete(bridge.udpEndpoints, id)
	}
	pending := make([]intercept.NativeID, 0, len(bridge.udpSockets))
	for id := range bridge.udpSockets {
		pending = append(pending, id)
		delete(bridge.udpSockets, id)
	}
	for id, admission := range bridge.udpAdmissions {
		delete(bridge.udpAdmissions, id)
		close(admission.done)
	}
	return flows, endpoints, pending
}

func validUDPCreatedEvent(event nativeUDPCreatedEvent) bool {
	placeholder := event.Local == netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
	return (placeholder || validIPv4Endpoint(event.Local)) && event.ExecutablePath != "" &&
		strings.IndexByte(event.ExecutablePath, 0) < 0 && isAbsoluteWindowsPath(event.ExecutablePath)
}

func validUDPDatagram(remote netip.AddrPort, payload []byte) bool {
	return validIPv4Endpoint(remote) && len(payload) <= nativeUDPMaxPayload
}

func validUDPSendEvent(event nativeUDPSendEvent) bool {
	return validIPv4Endpoint(event.Local) && validUDPDatagram(event.Remote, event.Payload)
}
