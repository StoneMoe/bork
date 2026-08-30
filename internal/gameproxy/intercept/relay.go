package intercept

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

type flowKind uint8

const (
	tcpFlow flowKind = iota + 1
	udpFlow
)

type flowKey struct {
	kind     flowKind
	nativeID NativeID
}

type activeFlow struct {
	cancel     context.CancelCauseFunc
	lastActive time.Time
	done       chan struct{}
}

type Relay struct {
	options Options

	mu      sync.Mutex
	state   GenerationState
	flows   map[flowKey]*activeFlow
	started bool
	closed  bool
	errors  []error

	closeOnce sync.Once
	closeErr  error
}

func New(options Options) (*Relay, error) {
	if options.Bridge == nil || options.Rules == nil || options.Dialer == nil ||
		!options.DNS.Is4() || options.QueueSize < 1 || options.IdleTimeout <= 0 || options.Clock == nil {
		return nil, ErrInvalidOptions
	}
	return &Relay{options: options, flows: make(map[flowKey]*activeFlow)}, nil
}

func (relay *Relay) SetState(state GenerationState) {
	relay.mu.Lock()
	changed := relay.state != state
	relay.state = state
	var active []*activeFlow
	if changed {
		active = relay.takeFlowsLocked()
	}
	relay.mu.Unlock()
	cause := error(ErrStaleGeneration)
	if !state.Ready {
		cause = ErrNotReady
	}
	for _, flow := range active {
		flow.cancel(cause)
	}
	waitFlows(active)
}

func (relay *Relay) GenerationState() GenerationState {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return relay.state
}

func (relay *Relay) Start(ctx context.Context) error {
	relay.mu.Lock()
	if relay.started {
		relay.mu.Unlock()
		return ErrRelayStarted
	}
	relay.started = true
	relay.mu.Unlock()
	return relay.options.Bridge.Start(ctx, relay)
}

func (relay *Relay) Run(ctx context.Context) error {
	err := relay.options.Bridge.Wait(ctx)
	closeErr := relay.Close()
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), closeErr)
	}
	if err == nil {
		err = ErrBridgeStopped
	}
	return errors.Join(&BridgeError{Cause: err}, closeErr)
}

func (relay *Relay) Close() error {
	relay.closeOnce.Do(func() {
		relay.mu.Lock()
		relay.closed = true
		active := relay.takeFlowsLocked()
		relay.mu.Unlock()
		for _, flow := range active {
			flow.cancel(net.ErrClosed)
		}
		bridgeErr := relay.options.Bridge.Close()
		waitFlows(active)
		relay.mu.Lock()
		relay.closeErr = errors.Join(bridgeErr, errors.Join(relay.errors...))
		relay.mu.Unlock()
	})
	return relay.closeErr
}

func (relay *Relay) match(metadata Metadata) error {
	selected, err := relay.options.Rules.Match(metadata.ExecutablePath)
	if err != nil {
		return &FlowError{NativeID: metadata.NativeID, Operation: "match executable", Cause: err}
	}
	if !selected {
		return &FlowError{NativeID: metadata.NativeID, Operation: "match executable", Cause: ErrUnselected}
	}
	if !metadata.OriginalLocal.Addr().Is4() || !metadata.OriginalRemote.Addr().Is4() ||
		!metadata.OriginalLocal.IsValid() || !metadata.OriginalRemote.IsValid() {
		return &FlowError{NativeID: metadata.NativeID, Operation: "validate endpoints", Cause: ErrInvalidFlow}
	}
	return nil
}

func (relay *Relay) beginFlow(
	parent context.Context,
	kind flowKind,
	metadata Metadata,
) (context.Context, *activeFlow, error) {
	now := relay.options.Clock.Now()
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.closed || !relay.state.Ready {
		return nil, nil, &FlowError{NativeID: metadata.NativeID, Operation: "admit", Cause: ErrNotReady}
	}
	if relay.state.Generation != metadata.Generation {
		return nil, nil, &FlowError{NativeID: metadata.NativeID, Operation: "admit", Cause: ErrStaleGeneration}
	}
	key := flowKey{kind: kind, nativeID: metadata.NativeID}
	if _, exists := relay.flows[key]; exists {
		return nil, nil, &FlowError{NativeID: metadata.NativeID, Operation: "admit", Cause: ErrDuplicateFlow}
	}
	ctx, cancel := context.WithCancelCause(parent)
	flow := &activeFlow{cancel: cancel, lastActive: now, done: make(chan struct{})}
	relay.flows[key] = flow
	return ctx, flow, nil
}

func (relay *Relay) ExpireIdle(now time.Time) int {
	relay.mu.Lock()
	var expired []*activeFlow
	for key, flow := range relay.flows {
		if key.kind == udpFlow && !flow.lastActive.Add(relay.options.IdleTimeout).After(now) {
			expired = append(expired, flow)
			delete(relay.flows, key)
		}
	}
	relay.mu.Unlock()
	for _, flow := range expired {
		flow.cancel(ErrIdle)
	}
	waitFlows(expired)
	return len(expired)
}

func (relay *Relay) finishFlow(kind flowKind, nativeID NativeID, finished *activeFlow) {
	relay.mu.Lock()
	key := flowKey{kind: kind, nativeID: nativeID}
	if relay.flows[key] == finished {
		delete(relay.flows, key)
	}
	relay.mu.Unlock()
	close(finished.done)
}

func (relay *Relay) touch(flow *activeFlow) {
	now := relay.options.Clock.Now()
	relay.mu.Lock()
	flow.lastActive = now
	relay.mu.Unlock()
}

func (relay *Relay) recordError(err error) {
	if err == nil {
		return
	}
	relay.mu.Lock()
	relay.errors = append(relay.errors, err)
	relay.mu.Unlock()
}

func (relay *Relay) takeFlowsLocked() []*activeFlow {
	active := make([]*activeFlow, 0, len(relay.flows))
	for key, flow := range relay.flows {
		active = append(active, flow)
		delete(relay.flows, key)
	}
	return active
}

func waitFlows(flows []*activeFlow) {
	for _, flow := range flows {
		<-flow.done
	}
}

func rejectTCP(flow NativeTCPFlow, cause error) error {
	return errors.Join(cause, resetAndCloseTCP(flow, cause))
}

func rejectUDP(endpoint NativeUDPEndpoint, cause error) error {
	return errors.Join(cause, resetAndCloseUDP(endpoint, cause))
}

func resetAndCloseTCP(flow NativeTCPFlow, cause error) error {
	return errors.Join(wrapClose("reset native TCP", flow.Reset(cause)), wrapClose("close native TCP", flow.Close()))
}

func resetAndCloseUDP(endpoint NativeUDPEndpoint, cause error) error {
	return errors.Join(wrapClose("reset native UDP", endpoint.Reset(cause)), wrapClose("close native UDP", endpoint.Close()))
}

func wrapClose(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
