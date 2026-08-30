package intercept

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
)

const maxUDPPayload = 65507

type udpSession struct {
	relay      *Relay
	native     NativeUDPEndpoint
	metadata   Metadata
	active     *activeFlow
	packetConn net.PacketConn
}

func (relay *Relay) UDP(ctx context.Context, endpoint NativeUDPEndpoint) error {
	metadata := endpoint.Metadata()
	if err := relay.match(metadata); err != nil {
		return rejectUDP(endpoint, err)
	}
	flowCtx, active, err := relay.beginFlow(ctx, udpFlow, metadata)
	if err != nil {
		return rejectUDP(endpoint, err)
	}
	session := &udpSession{relay: relay, native: endpoint, metadata: metadata, active: active}
	go session.run(flowCtx)
	return nil
}

func (session *udpSession) run(ctx context.Context) {
	defer session.relay.finishFlow(udpFlow, session.metadata.NativeID, session.active)
	packetConn, err := session.relay.options.Dialer.OpenUDP()
	if err != nil {
		failure := &FlowError{NativeID: session.metadata.NativeID, Operation: "open UDP", Cause: errors.Join(ErrDial, err)}
		session.relay.recordError(resetAndCloseUDP(session.native, failure))
		return
	}
	session.packetConn = packetConn

	workerCtx, cancel := context.WithCancelCause(ctx)
	toStack := make(chan Datagram, session.relay.options.QueueSize)
	toNative := make(chan Datagram, session.relay.options.QueueSize)
	results := make(chan error, 4)
	go func() { results <- session.readNative(workerCtx, toStack) }()
	go func() { results <- session.writeStack(workerCtx, toStack) }()
	go func() { results <- session.readStack(workerCtx, toNative) }()
	go func() { results <- session.writeNative(workerCtx, toNative) }()

	firstErr := <-results
	if firstErr == nil {
		firstErr = context.Canceled
	}
	cancel(firstErr)
	reset := !errors.Is(firstErr, context.Canceled) && !errors.Is(firstErr, net.ErrClosed)
	var resetErr error
	if reset {
		resetErr = wrapClose("reset native UDP", session.native.Reset(firstErr))
	}
	closeErr := errors.Join(
		wrapClose("close stack UDP", session.packetConn.Close()),
		wrapClose("close native UDP", session.native.Close()),
	)
	for range 3 {
		<-results
	}
	session.relay.recordError(errors.Join(resetErr, closeErr))
}

func (session *udpSession) readNative(ctx context.Context, queue chan<- Datagram) error {
	for {
		datagram, err := session.native.ReadDatagram(ctx)
		if err != nil {
			return session.packetError(ctx, "read native UDP", err)
		}
		if err := session.relay.validateDatagram(session.metadata, datagram.Metadata); err != nil {
			return err
		}
		datagram.Payload = slices.Clone(datagram.Payload)
		session.relay.touch(session.active)
		select {
		case queue <- datagram:
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
			return &FlowError{NativeID: session.metadata.NativeID, Operation: "queue native UDP", Cause: ErrQueueFull}
		}
	}
}

func (session *udpSession) writeStack(ctx context.Context, queue <-chan Datagram) error {
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case datagram := <-queue:
			destination := rewriteDNS(datagram.Metadata.OriginalRemote, session.relay.options.DNS)
			udpAddress := net.UDPAddrFromAddrPort(destination)
			written, err := session.packetConn.WriteTo(datagram.Payload, udpAddress)
			if err != nil {
				return session.packetError(ctx, "write stack UDP", err)
			}
			if written != len(datagram.Payload) {
				return session.packetError(ctx, "write stack UDP", io.ErrShortWrite)
			}
			session.relay.touch(session.active)
		}
	}
}

func (session *udpSession) readStack(ctx context.Context, queue chan<- Datagram) error {
	buffer := make([]byte, maxUDPPayload)
	for {
		count, source, err := session.packetConn.ReadFrom(buffer)
		if err != nil {
			return session.packetError(ctx, "read stack UDP", err)
		}
		sourceEndpoint, err := ipv4AddrPort(source)
		if err != nil {
			return &FlowError{NativeID: session.metadata.NativeID, Operation: "read stack UDP source", Cause: err}
		}
		if sourceEndpoint == netip.AddrPortFrom(session.relay.options.DNS, 53) && session.metadata.OriginalRemote.Port() == 53 {
			sourceEndpoint = session.metadata.OriginalRemote
		}
		metadata := session.metadata
		metadata.OriginalRemote = sourceEndpoint
		datagram := Datagram{Metadata: metadata, Payload: slices.Clone(buffer[:count])}
		session.relay.touch(session.active)
		select {
		case queue <- datagram:
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
			return &FlowError{NativeID: session.metadata.NativeID, Operation: "queue stack UDP", Cause: ErrQueueFull}
		}
	}
}

func (session *udpSession) writeNative(ctx context.Context, queue <-chan Datagram) error {
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case datagram := <-queue:
			datagram.Payload = slices.Clone(datagram.Payload)
			if err := session.native.WriteDatagram(ctx, datagram); err != nil {
				return session.packetError(ctx, "write native UDP", err)
			}
			session.relay.touch(session.active)
		}
	}
}

func (relay *Relay) validateDatagram(base, metadata Metadata) error {
	if metadata.Generation != base.Generation || metadata.NativeID != base.NativeID ||
		metadata.ProcessID != base.ProcessID || metadata.ExecutablePath != base.ExecutablePath ||
		metadata.OriginalLocal != base.OriginalLocal || !metadata.OriginalRemote.IsValid() ||
		!metadata.OriginalRemote.Addr().Is4() {
		return &FlowError{NativeID: base.NativeID, Operation: "validate UDP datagram", Cause: ErrInvalidFlow}
	}
	relay.mu.Lock()
	current := relay.state.Ready && relay.state.Generation == metadata.Generation
	relay.mu.Unlock()
	if !current {
		return &FlowError{NativeID: base.NativeID, Operation: "validate UDP generation", Cause: ErrStaleGeneration}
	}
	return nil
}

func (session *udpSession) packetError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return &FlowError{NativeID: session.metadata.NativeID, Operation: operation, Cause: errors.Join(ErrPacket, err)}
}

func ipv4AddrPort(address net.Addr) (netip.AddrPort, error) {
	udpAddress, ok := address.(*net.UDPAddr)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("UDP source %T: %w", address, ErrInvalidFlow)
	}
	endpoint := udpAddress.AddrPort()
	if !endpoint.IsValid() || !endpoint.Addr().Unmap().Is4() {
		return netip.AddrPort{}, fmt.Errorf("UDP source %v: %w", address, ErrInvalidFlow)
	}
	return netip.AddrPortFrom(endpoint.Addr().Unmap(), endpoint.Port()), nil
}
