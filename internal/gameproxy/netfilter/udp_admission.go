package netfilter

import (
	"bork/internal/gameproxy/intercept"
)

type udpAdmission struct {
	done   chan struct{}
	socket nativeUDPCreatedEvent
	send   nativeUDPSendEvent
}

func (bridge *Bridge) finishUDPAdmission(admission *udpAdmission, state intercept.GenerationState) {
	bridge.mu.Lock()
	send := admission.send
	current, exists := bridge.udpSockets[send.ID]
	active := bridge.state == bridgeStarting || bridge.state == bridgeStarted
	if !active || bridge.udpAdmissions[send.ID] != admission {
		bridge.mu.Unlock()
		return
	}
	if endpoint := bridge.udpEndpoints[send.ID]; endpoint != nil {
		delete(bridge.udpSockets, send.ID)
		bridge.finishUDPAdmissionLocked(send.ID, admission)
		bridge.mu.Unlock()
		if endpoint.metadata.OriginalLocal != send.Local {
			bridge.rejectUDPEndpoint(endpoint, send.ID, intercept.ErrInvalidFlow)
			return
		}
		bridge.enqueueUDP(endpoint, send.Remote, send.Payload)
		return
	}
	if !exists || current != admission.socket {
		bridge.finishUDPAdmissionLocked(send.ID, admission)
		bridge.mu.Unlock()
		return
	}
	if !state.Ready {
		delete(bridge.udpSockets, send.ID)
		bridge.finishUDPAdmissionLocked(send.ID, admission)
		bridge.mu.Unlock()
		bridge.recordCallbackError(bridge.backend.SuspendUDP(send.ID))
		return
	}
	metadata := intercept.Metadata{
		Generation: state.Generation, NativeID: send.ID, ProcessID: admission.socket.PID,
		ExecutablePath: admission.socket.ExecutablePath, OriginalLocal: send.Local, OriginalRemote: send.Remote,
	}
	callbacks := bridge.callbacks
	ctx := bridge.startCtx
	endpoint := newUDPEndpoint(ctx, bridge.backend, metadata)
	bridge.udpEndpoints[send.ID] = endpoint
	delete(bridge.udpSockets, send.ID)
	_ = endpoint.enqueue(send.Remote, send.Payload)
	bridge.finishUDPAdmissionLocked(send.ID, admission)
	bridge.mu.Unlock()

	if err := callbacks.UDP(ctx, endpoint); err != nil {
		bridge.recordCallbackError(endpoint.Reset(err))
	}
}

func (bridge *Bridge) finishUDPAdmissionLocked(id intercept.NativeID, admission *udpAdmission) {
	if admission == nil || bridge.udpAdmissions[id] != admission {
		return
	}
	delete(bridge.udpAdmissions, id)
	close(admission.done)
}
