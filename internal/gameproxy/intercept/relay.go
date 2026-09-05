//go:build game_proxy

package intercept

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type flowKind uint8

const (
	tcpFlow flowKind = iota + 1
	udpFlow
	maxQueuedFlowEvents = 4096
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

	closeOnce         sync.Once
	closeErr          error
	uploadBytes       atomic.Uint64
	downloadBytes     atomic.Uint64
	trafficMu         sync.Mutex
	trafficAt         time.Time
	trafficUpload     uint64
	trafficDownload   uint64
	flowEvents        chan FlowEvent
	flowEventStop     chan struct{}
	flowEventDone     chan struct{}
	flowEventOnce     sync.Once
	droppedFlowEvents atomic.Uint64
}

func (relay *Relay) SetRules(rules ExecutableMatcher) error {
	if rules == nil {
		return ErrInvalidOptions
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.closed {
		return ErrRelayClosed
	}
	relay.options.Rules = rules
	return nil
}

func (relay *Relay) Traffic() TrafficStats {
	relay.trafficMu.Lock()
	defer relay.trafficMu.Unlock()
	now := relay.options.Clock.Now()
	upload := relay.uploadBytes.Load()
	download := relay.downloadBytes.Load()
	var uploadRate, downloadRate uint64
	if !relay.trafficAt.IsZero() {
		seconds := now.Sub(relay.trafficAt).Seconds()
		if seconds > 0 {
			uploadRate = uint64(float64(upload-relay.trafficUpload) / seconds)
			downloadRate = uint64(float64(download-relay.trafficDownload) / seconds)
		}
	}
	relay.trafficAt, relay.trafficUpload, relay.trafficDownload = now, upload, download
	return TrafficStats{UploadBytes: upload, DownloadBytes: download, UploadRate: uploadRate, DownloadRate: downloadRate}
}

func (relay *Relay) addUpload(bytes int)   { relay.uploadBytes.Add(uint64(bytes)) }
func (relay *Relay) addDownload(bytes int) { relay.downloadBytes.Add(uint64(bytes)) }

func (relay *Relay) emitFlowEvent(event FlowEvent) {
	if relay.options.OnFlowEvent == nil {
		return
	}
	select {
	case relay.flowEvents <- event:
	case <-relay.flowEventStop:
	default:
		relay.droppedFlowEvents.Add(1)
	}
}

func New(options Options) (*Relay, error) {
	if options.Bridge == nil || options.Rules == nil || options.Dialer == nil ||
		!options.DNS.Is4() || options.QueueSize < 1 || options.IdleTimeout <= 0 || options.Clock == nil {
		return nil, ErrInvalidOptions
	}
	relay := &Relay{
		options: options, flows: make(map[flowKey]*activeFlow),
		flowEvents:    make(chan FlowEvent, maxQueuedFlowEvents),
		flowEventStop: make(chan struct{}), flowEventDone: make(chan struct{}),
	}
	go relay.dispatchFlowEvents()
	return relay, nil
}

func (relay *Relay) dispatchFlowEvents() {
	defer close(relay.flowEventDone)
	for {
		select {
		case event := <-relay.flowEvents:
			relay.invokeFlowEvent(event)
		case <-relay.flowEventStop:
			for {
				select {
				case event := <-relay.flowEvents:
					relay.invokeFlowEvent(event)
				default:
					return
				}
			}
		}
	}
}

func (relay *Relay) invokeFlowEvent(event FlowEvent) {
	relay.callFlowEvent(event)
	if dropped := relay.droppedFlowEvents.Swap(0); dropped > 0 {
		relay.callFlowEvent(FlowEvent{RequestID: "flow-events", Kind: FlowEventDropped, Bytes: dropped})
	}
}

func (relay *Relay) callFlowEvent(event FlowEvent) {
	defer func() { _ = recover() }()
	relay.options.OnFlowEvent(event)
}

func (relay *Relay) stopFlowEvents() {
	relay.flowEventOnce.Do(func() {
		close(relay.flowEventStop)
		<-relay.flowEventDone
	})
}

func (relay *Relay) SetState(state GenerationState) {
	relay.mu.Lock()
	changed := relay.state != state
	relay.state = state
	var active []*activeFlow
	if changed {
		active = relay.snapshotFlowsLocked()
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

func (relay *Relay) EndpointError(err error) {
	if err != nil && relay.options.OnEndpointError != nil {
		relay.options.OnEndpointError(err)
	}
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
		active := relay.snapshotFlowsLocked()
		relay.mu.Unlock()
		for _, flow := range active {
			flow.cancel(net.ErrClosed)
		}
		bridgeErr := relay.options.Bridge.Close()
		waitFlows(active)
		relay.stopFlowEvents()
		relay.closeErr = bridgeErr
	})
	return relay.closeErr
}

func (relay *Relay) match(metadata Metadata) error {
	if metadata.ExecutablePath == "" {
		if metadata.NativeRuleMatched {
			return relay.validateEndpoints(metadata)
		}
		return &FlowError{NativeID: metadata.NativeID, Operation: "match executable", Cause: ErrUnselected}
	}
	relay.mu.Lock()
	rules := relay.options.Rules
	relay.mu.Unlock()
	selected, err := rules.Match(metadata.ExecutablePath)
	if err != nil {
		return &FlowError{NativeID: metadata.NativeID, Operation: "match executable", Cause: err}
	}
	if !selected {
		return &FlowError{NativeID: metadata.NativeID, Operation: "match executable", Cause: ErrUnselected}
	}
	return relay.validateEndpoints(metadata)
}

func (*Relay) validateEndpoints(metadata Metadata) error {
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
	finished.cancel(nil)
	relay.mu.Lock()
	key := flowKey{kind: kind, nativeID: nativeID}
	if relay.flows[key] == finished {
		delete(relay.flows, key)
	}
	close(finished.done)
	relay.mu.Unlock()
}

func (relay *Relay) touch(flow *activeFlow) {
	now := relay.options.Clock.Now()
	relay.mu.Lock()
	flow.lastActive = now
	relay.mu.Unlock()
}

func (relay *Relay) snapshotFlowsLocked() []*activeFlow {
	active := make([]*activeFlow, 0, len(relay.flows))
	for _, flow := range relay.flows {
		active = append(active, flow)
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
