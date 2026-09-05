//go:build game_proxy

package netfilter

import "errors"

func (bridge *Bridge) endpointError(failure *NativeCallbackError) {
	if failure == nil {
		return
	}
	bridge.mu.Lock()
	if bridge.state != bridgeStarting && bridge.state != bridgeStarted {
		bridge.mu.Unlock()
		return
	}
	bridge.callbackWG.Add(1)
	protocol := failure.Event.protocol()
	var pending *tcpRedirectRequest
	var redirected *redirectedTCPFlow
	var flow *tcpFlow
	var endpoint *udpEndpoint
	var pendingUDP bool
	switch protocol {
	case nativeProtocolTCP:
		pending = bridge.tcpPending[failure.ID]
		redirected = bridge.tcpRedirects[failure.ID]
		flow = bridge.flows[failure.ID]
		delete(bridge.tcpPending, failure.ID)
		delete(bridge.tcpRedirects, failure.ID)
		delete(bridge.flows, failure.ID)
	case nativeProtocolUDP:
		endpoint = bridge.udpEndpoints[failure.ID]
		_, hasSocket := bridge.udpSockets[failure.ID]
		admission := bridge.udpAdmissions[failure.ID]
		pendingUDP = hasSocket || admission != nil
		delete(bridge.udpEndpoints, failure.ID)
		delete(bridge.udpSockets, failure.ID)
		bridge.finishUDPAdmissionLocked(failure.ID, admission)
	default:
		bridge.mu.Unlock()
		bridge.callbackWG.Done()
		bridge.reportFatal(failure)
		bridge.recordCallbackError(failure)
		return
	}
	bridge.mu.Unlock()
	defer bridge.callbackWG.Done()

	var cleanupErr error
	failureDelivered := false
	if pending != nil {
		cleanupErr = errors.Join(cleanupErr, pending.listener.Close())
	}
	cleanupSucceeded := failure.CleanupStatus == nativeStatusSuccess
	if redirected != nil {
		cleanupErr = errors.Join(cleanupErr, redirected.Reset(failure))
	}
	if flow != nil {
		if cleanupSucceeded {
			failureDelivered = flow.nativeFailed(failure)
		} else {
			cleanupErr = errors.Join(cleanupErr, flow.Reset(failure))
		}
	} else if protocol == nativeProtocolUDP && pendingUDP && !cleanupSucceeded &&
		!isBackendFatalStatus(failure.Status) && !isBackendFatalStatus(failure.CleanupStatus) {
		cleanupErr = errors.Join(cleanupErr, bridge.backend.SuspendUDP(failure.ID))
	}
	if endpoint != nil {
		if cleanupSucceeded || isBackendFatalStatus(failure.Status) || isBackendFatalStatus(failure.CleanupStatus) {
			failureDelivered = endpoint.nativeFailed(failure) || failureDelivered
		} else {
			cleanupErr = errors.Join(cleanupErr, endpoint.Reset(failure))
		}
	}
	if failureDelivered {
		bridge.recordCallbackError(cleanupErr)
	} else {
		bridge.recordCallbackError(errors.Join(error(failure), cleanupErr))
	}
}
