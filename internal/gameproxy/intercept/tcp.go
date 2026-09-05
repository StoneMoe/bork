package intercept

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
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
	relay         *Relay
	native        NativeTCPFlow
	metadata      Metadata
	active        *activeFlow
	requestID     string
	uploadBytes   atomic.Uint64
	downloadBytes atomic.Uint64
	responseOnce  sync.Once
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
	session := &tcpSession{
		relay: relay, native: flow, metadata: metadata, active: active,
		requestID: "tcp-" + strconv.FormatUint(uint64(metadata.NativeID), 10),
	}
	go session.run(flowCtx)
	return nil
}

func (session *tcpSession) run(ctx context.Context) {
	defer session.relay.finishFlow(tcpFlow, session.metadata.NativeID, session.active)
	remote := rewriteDNS(session.metadata.OriginalRemote, session.relay.options.DNS)
	session.relay.emitFlowEvent(FlowEvent{
		RequestID: session.requestID, Protocol: "TCP", Kind: FlowEventRequest, Metadata: session.metadata,
	})
	stack, err := session.relay.options.Dialer.DialTCP(ctx, remote)
	if err != nil {
		failure := &FlowError{NativeID: session.metadata.NativeID, Operation: "dial TCP", Cause: errors.Join(ErrDial, err)}
		failureErr := errors.Join(failure, resetAndCloseTCP(session.native, failure))
		if stoppedByCancellation(ctx) {
			session.complete(nil)
			return
		}
		session.relay.emitFlowEvent(FlowEvent{
			RequestID: session.requestID, Protocol: "TCP", Kind: FlowEventFailed,
			Metadata: session.metadata, Err: failureErr,
		})
		return
	}

	copyErr := session.relay.relayTCP(ctx, session.native, stack, session.countUpload, session.countDownload)
	if copyErr != nil {
		if stoppedByCancellation(ctx) {
			session.complete(nil)
		} else {
			session.complete(copyErr)
		}
		return
	}
	session.complete(errors.Join(
		wrapClose("close native TCP", session.native.Close()),
		wrapClose("close stack TCP", stack.Close()),
	))
}

func stoppedByCancellation(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), context.Canceled)
}

func (relay *Relay) relayTCP(
	ctx context.Context,
	native NativeTCPFlow,
	stack net.Conn,
	countUpload func(int),
	countDownload func(int),
) error {
	results := make(chan tcpCopyResult, 2)
	go copyTCP(stack, native, results, countUpload)
	go copyTCP(native, stack, results, countDownload)

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

func (session *tcpSession) countUpload(bytes int) {
	session.uploadBytes.Add(uint64(bytes))
	session.relay.addUpload(bytes)
}

func (session *tcpSession) countDownload(bytes int) {
	session.downloadBytes.Add(uint64(bytes))
	session.relay.addDownload(bytes)
	session.responseOnce.Do(func() {
		session.relay.emitFlowEvent(FlowEvent{
			RequestID: session.requestID, Protocol: "TCP", Kind: FlowEventResponse,
			Metadata: session.metadata, Bytes: uint64(bytes),
		})
	})
}

func (session *tcpSession) complete(err error) {
	session.relay.emitFlowEvent(FlowEvent{
		RequestID: session.requestID, Protocol: "TCP", Kind: FlowEventComplete,
		Metadata: session.metadata, UploadBytes: session.uploadBytes.Load(),
		DownloadBytes: session.downloadBytes.Load(), Err: err,
	})
}

func (relay *Relay) failTCPRelay(native NativeTCPFlow, stack net.Conn, cause error) error {
	failure := &FlowError{NativeID: native.Metadata().NativeID, Operation: "relay TCP", Cause: cause}
	return errors.Join(failure,
		wrapClose("reset native TCP", native.Reset(failure)),
		wrapClose("close native TCP", native.Close()),
		wrapClose("close stack TCP", stack.Close()),
	)
}

func copyTCP(destination io.Writer, source io.Reader, results chan<- tcpCopyResult, count func(int)) {
	_, err := io.Copy(countingWriter{Writer: destination, count: count}, source)
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

type countingWriter struct {
	io.Writer
	count func(int)
}

func (writer countingWriter) Write(payload []byte) (int, error) {
	written, err := writer.Writer.Write(payload)
	if written > 0 {
		writer.count(written)
	}
	return written, err
}

func rewriteDNS(original netip.AddrPort, dns netip.Addr) netip.AddrPort {
	if original.Port() == 53 {
		return netip.AddrPortFrom(dns, 53)
	}
	return original
}
