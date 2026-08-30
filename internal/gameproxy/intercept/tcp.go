package intercept

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
)

type closeWriter interface {
	CloseWrite() error
}

type closeReader interface {
	CloseRead() error
}

type tcpCopyResult struct {
	err error
}

type tcpSession struct {
	relay    *Relay
	native   NativeTCPFlow
	metadata Metadata
	active   *activeFlow
}

func (relay *Relay) TCP(ctx context.Context, flow NativeTCPFlow) error {
	metadata := flow.Metadata()
	if err := relay.match(metadata); err != nil {
		return rejectTCP(flow, err)
	}
	flowCtx, active, err := relay.beginFlow(ctx, tcpFlow, metadata)
	if err != nil {
		return rejectTCP(flow, err)
	}
	session := &tcpSession{relay: relay, native: flow, metadata: metadata, active: active}
	go session.run(flowCtx)
	return nil
}

func (session *tcpSession) run(ctx context.Context) {
	defer session.relay.finishFlow(tcpFlow, session.metadata.NativeID, session.active)
	remote := rewriteDNS(session.metadata.OriginalRemote, session.relay.options.DNS)
	stack, err := session.relay.options.Dialer.DialTCP(ctx, remote)
	if err != nil {
		failure := &FlowError{NativeID: session.metadata.NativeID, Operation: "dial TCP", Cause: errors.Join(ErrDial, err)}
		session.relay.recordError(resetAndCloseTCP(session.native, failure))
		return
	}

	copyErr := session.relay.relayTCP(ctx, session.native, stack)
	if copyErr != nil {
		session.relay.recordError(errors.Join(
			wrapClose("close native TCP", session.native.Close()),
			wrapClose("close stack TCP", stack.Close()),
		))
		return
	}
	session.relay.recordError(errors.Join(
		wrapClose("close native TCP", session.native.Close()),
		wrapClose("close stack TCP", stack.Close()),
	))
}

func (relay *Relay) relayTCP(ctx context.Context, native NativeTCPFlow, stack net.Conn) error {
	results := make(chan tcpCopyResult, 2)
	go copyTCP(stack, native, results)
	go copyTCP(native, stack, results)

	completed := 0
	var relayErr error
	for completed < 2 {
		if relayErr != nil {
			<-results
			completed++
			continue
		}
		select {
		case <-ctx.Done():
			relayErr = relay.failTCPRelay(native, stack, context.Cause(ctx))
		case copyResult := <-results:
			completed++
			if copyResult.err != nil {
				relayErr = relay.failTCPRelay(native, stack, copyResult.err)
			}
		}
	}
	return relayErr
}

func (relay *Relay) failTCPRelay(native NativeTCPFlow, stack net.Conn, cause error) error {
	failure := &FlowError{NativeID: native.Metadata().NativeID, Operation: "relay TCP", Cause: cause}
	relay.recordError(errors.Join(
		wrapClose("reset native TCP", native.Reset(failure)),
		wrapClose("close native TCP", native.Close()),
		wrapClose("close stack TCP", stack.Close()),
	))
	return failure
}

func copyTCP(destination io.Writer, source io.Reader, results chan<- tcpCopyResult) {
	_, err := io.Copy(destination, source)
	if err == nil {
		if closer, ok := destination.(closeWriter); ok {
			err = closer.CloseWrite()
		}
		if closer, ok := source.(closeReader); ok {
			err = errors.Join(err, closer.CloseRead())
		}
	}
	if err != nil {
		err = fmt.Errorf("copy stream: %w", err)
	}
	results <- tcpCopyResult{err: err}
}

func rewriteDNS(original netip.AddrPort, dns netip.Addr) netip.AddrPort {
	if original.Port() == 53 {
		return netip.AddrPortFrom(dns, 53)
	}
	return original
}
