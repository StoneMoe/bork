package intercept

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const maxUDPPayload = 65507
const maxPendingUDPTraceRequests = 1024
const maxDNSPacketConns = 64
const udpTraceRequestTimeout = 30 * time.Second
const udpTrafficLogInterval = 5 * time.Second

type pendingUDPTraceRequest struct {
	id             string
	remote         netip.AddrPort
	originalRemote netip.AddrPort
	createdAt      time.Time
	transactionID  uint16
}

type queuedNativeDatagram struct {
	datagram  Datagram
	requestID string
	dns       bool
}

type udpTrafficSnapshot struct {
	uploadPackets   uint64
	uploadBytes     uint64
	downloadPackets uint64
	downloadBytes   uint64
}

type udpSession struct {
	relay           *Relay
	native          NativeUDPEndpoint
	metadata        Metadata
	active          *activeFlow
	packetConn      net.PacketConn
	flowID          string
	uploadPackets   atomic.Uint64
	uploadBytes     atomic.Uint64
	downloadPackets atomic.Uint64
	downloadBytes   atomic.Uint64

	trafficMu      sync.Mutex
	genericTraffic udpTrafficSnapshot
	loggedTraffic  udpTrafficSnapshot
	outboundOnce   sync.Once
	inboundOnce    sync.Once

	traceMu       sync.Mutex
	traceSequence uint64
	tracePending  []pendingUDPTraceRequest
	traceDropped  atomic.Uint64

	dnsMu          sync.Mutex
	dnsPacketConns map[netip.AddrPort]net.PacketConn
	dnsReaders     sync.WaitGroup
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
	session := &udpSession{
		relay: relay, native: endpoint, metadata: metadata, active: active,
		flowID: udpFlowID(metadata.NativeID),
	}
	go session.run(flowCtx)
	return nil
}

func (session *udpSession) run(ctx context.Context) {
	defer session.relay.finishFlow(udpFlow, session.metadata.NativeID, session.active)
	packetConn, err := session.relay.options.Dialer.OpenUDP()
	if err != nil {
		failure := &FlowError{NativeID: session.metadata.NativeID, Operation: "open UDP", Cause: errors.Join(ErrDial, err)}
		failureErr := errors.Join(failure, resetAndCloseUDP(session.native, failure))
		if stoppedByCancellation(ctx) {
			session.emitComplete(nil)
			return
		}
		session.relay.emitFlowEvent(FlowEvent{
			FlowID: session.flowID, Protocol: "UDP", Kind: FlowEventFailed,
			Metadata: session.metadata, Err: failureErr,
		})
		return
	}
	session.packetConn = packetConn

	workerCtx, cancel := context.WithCancelCause(ctx)
	toStack := make(chan Datagram, session.relay.options.QueueSize)
	toNative := make(chan queuedNativeDatagram, session.relay.options.QueueSize)
	results := make(chan error, 4)
	dnsResults := make(chan error, 1)
	go func() { results <- session.readNative(workerCtx, toStack) }()
	go func() { results <- session.writeStack(workerCtx, toStack, toNative, dnsResults) }()
	go func() { results <- session.readStack(workerCtx, toNative) }()
	go func() { results <- session.writeNative(workerCtx, toNative) }()

	trafficTicker := time.NewTicker(udpTrafficLogInterval)
	defer trafficTicker.Stop()
	var firstErr error
	baseResult := true
waitForWorker:
	for {
		select {
		case firstErr = <-results:
			break waitForWorker
		case firstErr = <-dnsResults:
			baseResult = false
			break waitForWorker
		case <-trafficTicker.C:
			session.emitGenericTraffic()
			session.emitDroppedDNSTraces()
		}
	}
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
		session.closeDNSPacketConns(),
		wrapClose("close native UDP", session.native.Close()),
	)
	remaining := 4
	if baseResult {
		remaining--
	}
	for range remaining {
		<-results
	}
	session.dnsReaders.Wait()
	session.emitDroppedDNSTraces()
	var sessionErr error
	if reset {
		sessionErr = firstErr
	}
	completionErr := errors.Join(sessionErr, resetErr, closeErr)
	if stoppedByCancellation(ctx) {
		completionErr = nil
	}
	session.emitComplete(completionErr)
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

func (session *udpSession) writeStack(
	ctx context.Context,
	queue <-chan Datagram,
	toNative chan<- queuedNativeDatagram,
	dnsResults chan<- error,
) error {
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case datagram := <-queue:
			originalRemote := datagram.Metadata.OriginalRemote
			destination := rewriteDNS(originalRemote, session.relay.options.DNS)
			udpAddress := net.UDPAddrFromAddrPort(destination)
			request, isDNS := session.addDNSTraceRequest(originalRemote, destination, datagram.Payload)
			packetConn := session.packetConn
			dnsRoute := originalRemote.Port() == 53 && destination == netip.AddrPortFrom(session.relay.options.DNS, 53)
			if dnsRoute {
				var err error
				packetConn, err = session.openDNSPacketConn(ctx, originalRemote, toNative, dnsResults)
				if err != nil {
					if isDNS {
						session.removeUDPTraceRequest(request.id)
					}
					if ctx.Err() != nil {
						return context.Cause(ctx)
					}
					return &FlowError{
						NativeID:  session.metadata.NativeID,
						Operation: "open DNS UDP",
						Cause:     errors.Join(ErrDial, err),
					}
				}
			}
			if isDNS {
				session.relay.emitFlowEvent(FlowEvent{
					RequestID: request.id, Protocol: "UDP", Kind: FlowEventRequest,
					Metadata: datagram.Metadata, Bytes: uint64(len(datagram.Payload)),
				})
			}
			written, err := packetConn.WriteTo(datagram.Payload, udpAddress)
			if err != nil {
				if isDNS {
					session.removeUDPTraceRequest(request.id)
				}
				return session.packetError(ctx, "write stack UDP", err)
			}
			if written != len(datagram.Payload) {
				if isDNS {
					session.removeUDPTraceRequest(request.id)
				}
				return session.packetError(ctx, "write stack UDP", io.ErrShortWrite)
			}
			session.relay.addUpload(written)
			session.uploadPackets.Add(1)
			session.uploadBytes.Add(uint64(written))
			if !isDNS {
				session.recordGenericUpload(datagram.Metadata, written)
			}
			session.relay.touch(session.active)
		}
	}
}

func (session *udpSession) openDNSPacketConn(
	ctx context.Context,
	originalRemote netip.AddrPort,
	toNative chan<- queuedNativeDatagram,
	dnsResults chan<- error,
) (net.PacketConn, error) {
	session.dnsMu.Lock()
	defer session.dnsMu.Unlock()
	if packetConn := session.dnsPacketConns[originalRemote]; packetConn != nil {
		return packetConn, nil
	}
	if len(session.dnsPacketConns) >= maxDNSPacketConns {
		return nil, ErrResourceLimit
	}
	packetConn, err := session.relay.options.Dialer.OpenUDP()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = packetConn.Close()
		return nil, err
	}
	if session.dnsPacketConns == nil {
		session.dnsPacketConns = make(map[netip.AddrPort]net.PacketConn)
	}
	session.dnsPacketConns[originalRemote] = packetConn
	session.dnsReaders.Add(1)
	go func() {
		defer session.dnsReaders.Done()
		err := session.readDNSStack(ctx, packetConn, originalRemote, toNative)
		select {
		case dnsResults <- err:
		case <-ctx.Done():
		}
	}()
	return packetConn, nil
}

func (session *udpSession) closeDNSPacketConns() error {
	session.dnsMu.Lock()
	packetConns := make([]net.PacketConn, 0, len(session.dnsPacketConns))
	for _, packetConn := range session.dnsPacketConns {
		packetConns = append(packetConns, packetConn)
	}
	session.dnsPacketConns = nil
	session.dnsMu.Unlock()
	var closeErr error
	for _, packetConn := range packetConns {
		closeErr = errors.Join(closeErr, wrapClose("close DNS stack UDP", packetConn.Close()))
	}
	return closeErr
}

func (session *udpSession) readStack(ctx context.Context, queue chan<- queuedNativeDatagram) error {
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
		metadata := session.metadata
		metadata.OriginalRemote = sourceEndpoint
		datagram := queuedNativeDatagram{
			datagram: Datagram{Metadata: metadata, Payload: slices.Clone(buffer[:count])},
		}
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

func (session *udpSession) readDNSStack(
	ctx context.Context,
	packetConn net.PacketConn,
	originalRemote netip.AddrPort,
	queue chan<- queuedNativeDatagram,
) error {
	buffer := make([]byte, maxUDPPayload)
	resolver := netip.AddrPortFrom(session.relay.options.DNS, 53)
	for {
		count, source, err := packetConn.ReadFrom(buffer)
		if err != nil {
			return session.packetError(ctx, "read DNS stack UDP", err)
		}
		sourceEndpoint, err := ipv4AddrPort(source)
		if err != nil {
			return &FlowError{NativeID: session.metadata.NativeID, Operation: "read DNS stack UDP source", Cause: err}
		}
		request, matched := session.takeDNSTraceRequest(originalRemote, sourceEndpoint, buffer[:count])
		fromResolver := sourceEndpoint == resolver
		if fromResolver {
			sourceEndpoint = originalRemote
		}
		metadata := session.metadata
		metadata.OriginalRemote = sourceEndpoint
		datagram := queuedNativeDatagram{
			datagram: Datagram{Metadata: metadata, Payload: slices.Clone(buffer[:count])},
		}
		if matched {
			datagram.requestID = request.id
		}
		datagram.dns = fromResolver
		session.relay.touch(session.active)
		select {
		case queue <- datagram:
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
			return &FlowError{NativeID: session.metadata.NativeID, Operation: "queue DNS stack UDP", Cause: ErrQueueFull}
		}
	}
}

func (session *udpSession) addDNSTraceRequest(
	originalRemote netip.AddrPort,
	remote netip.AddrPort,
	payload []byte,
) (pendingUDPTraceRequest, bool) {
	transactionID, ok := udpDNSQueryTransactionID(payload)
	if originalRemote.Port() != 53 || remote.Port() != 53 || !ok {
		return pendingUDPTraceRequest{}, false
	}
	session.traceMu.Lock()
	defer session.traceMu.Unlock()
	now := session.relay.options.Clock.Now()
	session.expireUDPTraceRequestsLocked(now)
	for index := range session.tracePending {
		request := &session.tracePending[index]
		if request.originalRemote == originalRemote && request.remote == remote && request.transactionID == transactionID {
			request.createdAt = now
			return *request, true
		}
	}
	if len(session.tracePending) == maxPendingUDPTraceRequests {
		session.traceDropped.Add(1)
		copy(session.tracePending, session.tracePending[1:])
		session.tracePending = session.tracePending[:len(session.tracePending)-1]
	}
	session.traceSequence++
	request := pendingUDPTraceRequest{
		id:     udpDNSRequestID(session.metadata.NativeID, session.traceSequence),
		remote: remote, originalRemote: originalRemote, createdAt: now, transactionID: transactionID,
	}
	session.tracePending = append(session.tracePending, request)
	return request, true
}

func (session *udpSession) removeUDPTraceRequest(requestID string) {
	session.traceMu.Lock()
	defer session.traceMu.Unlock()
	for index, request := range session.tracePending {
		if request.id == requestID {
			session.tracePending = append(session.tracePending[:index], session.tracePending[index+1:]...)
			return
		}
	}
}

func (session *udpSession) takeDNSTraceRequest(
	originalRemote netip.AddrPort,
	remote netip.AddrPort,
	payload []byte,
) (pendingUDPTraceRequest, bool) {
	transactionID, ok := udpDNSResponseTransactionID(payload)
	if remote.Port() != 53 || !ok {
		return pendingUDPTraceRequest{}, false
	}
	session.traceMu.Lock()
	defer session.traceMu.Unlock()
	session.expireUDPTraceRequestsLocked(session.relay.options.Clock.Now())
	for index, request := range session.tracePending {
		if request.originalRemote != originalRemote || request.remote != remote || request.transactionID != transactionID {
			continue
		}
		session.tracePending = append(session.tracePending[:index], session.tracePending[index+1:]...)
		return request, true
	}
	return pendingUDPTraceRequest{}, false
}

func (session *udpSession) expireUDPTraceRequestsLocked(now time.Time) {
	pending := session.tracePending[:0]
	for _, request := range session.tracePending {
		if now.Sub(request.createdAt) <= udpTraceRequestTimeout {
			pending = append(pending, request)
		}
	}
	session.tracePending = pending
}

func udpDNSQueryTransactionID(payload []byte) (uint16, bool) {
	return udpDNSTransactionID(payload, false)
}

func udpDNSResponseTransactionID(payload []byte) (uint16, bool) {
	return udpDNSTransactionID(payload, true)
}

func udpDNSTransactionID(payload []byte, response bool) (uint16, bool) {
	if len(payload) < 12 || (payload[2]&0x80 != 0) != response || binary.BigEndian.Uint16(payload[4:6]) == 0 {
		return 0, false
	}
	offset := 12
	for {
		if offset >= len(payload) {
			return 0, false
		}
		length := int(payload[offset])
		offset++
		if length == 0 {
			break
		}
		if length&0xc0 == 0xc0 {
			if offset >= len(payload) {
				return 0, false
			}
			offset++
			break
		}
		if length > 63 || offset+length > len(payload) {
			return 0, false
		}
		offset += length
	}
	if offset+4 > len(payload) {
		return 0, false
	}
	return binary.BigEndian.Uint16(payload), true
}

func udpDNSRequestID(nativeID NativeID, sequence uint64) string {
	return "udp-" + strconv.FormatUint(uint64(nativeID), 10) + "-dns-" + strconv.FormatUint(sequence, 10)
}

func udpFlowID(nativeID NativeID) string {
	return "udp-" + strconv.FormatUint(uint64(nativeID), 10)
}

func (session *udpSession) writeNative(ctx context.Context, queue <-chan queuedNativeDatagram) error {
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case queued := <-queue:
			datagram := queued.datagram
			datagram.Payload = slices.Clone(datagram.Payload)
			if err := session.native.WriteDatagram(ctx, datagram); err != nil {
				return session.packetError(ctx, "write native UDP", err)
			}
			session.relay.addDownload(len(datagram.Payload))
			session.downloadPackets.Add(1)
			session.downloadBytes.Add(uint64(len(datagram.Payload)))
			if queued.requestID != "" {
				session.relay.emitFlowEvent(FlowEvent{
					RequestID: queued.requestID, Protocol: "UDP", Kind: FlowEventResponse,
					Metadata: datagram.Metadata, Bytes: uint64(len(datagram.Payload)),
				})
			} else if !queued.dns {
				session.recordGenericDownload(datagram.Metadata, len(datagram.Payload))
			}
			session.relay.touch(session.active)
		}
	}
}

func (session *udpSession) emitGenericTraffic() {
	session.trafficMu.Lock()
	current := session.genericTraffic
	previous := session.loggedTraffic
	if current == previous {
		session.trafficMu.Unlock()
		return
	}
	session.loggedTraffic = current
	session.trafficMu.Unlock()
	session.relay.emitFlowEvent(FlowEvent{
		FlowID: session.flowID, Protocol: "UDP", Kind: FlowEventTraffic, Metadata: session.metadata,
		UploadPackets:   current.uploadPackets - previous.uploadPackets,
		UploadBytes:     current.uploadBytes - previous.uploadBytes,
		DownloadPackets: current.downloadPackets - previous.downloadPackets,
		DownloadBytes:   current.downloadBytes - previous.downloadBytes,
	})
}

func (session *udpSession) emitDroppedDNSTraces() {
	dropped := session.traceDropped.Swap(0)
	if dropped == 0 {
		return
	}
	session.relay.emitFlowEvent(FlowEvent{
		RequestID: session.flowID + "-dns-correlation", Kind: FlowEventDropped, Bytes: dropped,
	})
}

func (session *udpSession) recordGenericUpload(metadata Metadata, bytes int) {
	session.trafficMu.Lock()
	defer session.trafficMu.Unlock()
	session.genericTraffic.uploadPackets++
	session.genericTraffic.uploadBytes += uint64(bytes)
	session.outboundOnce.Do(func() {
		session.relay.emitFlowEvent(FlowEvent{
			FlowID: session.flowID, Protocol: "UDP", Kind: FlowEventOutbound,
			Metadata: metadata, Bytes: uint64(bytes),
		})
	})
}

func (session *udpSession) recordGenericDownload(metadata Metadata, bytes int) {
	session.trafficMu.Lock()
	defer session.trafficMu.Unlock()
	session.genericTraffic.downloadPackets++
	session.genericTraffic.downloadBytes += uint64(bytes)
	session.inboundOnce.Do(func() {
		session.relay.emitFlowEvent(FlowEvent{
			FlowID: session.flowID, Protocol: "UDP", Kind: FlowEventInbound,
			Metadata: metadata, Bytes: uint64(bytes),
		})
	})
}

func (session *udpSession) emitComplete(err error) {
	session.relay.emitFlowEvent(FlowEvent{
		FlowID: session.flowID, Protocol: "UDP", Kind: FlowEventComplete, Metadata: session.metadata,
		UploadPackets: session.uploadPackets.Load(), UploadBytes: session.uploadBytes.Load(),
		DownloadPackets: session.downloadPackets.Load(), DownloadBytes: session.downloadBytes.Load(), Err: err,
	})
}

func (relay *Relay) validateDatagram(base, metadata Metadata) error {
	if metadata.Generation != base.Generation || metadata.NativeID != base.NativeID ||
		metadata.ProcessID != base.ProcessID || metadata.ExecutablePath != base.ExecutablePath ||
		metadata.NativeRuleMatched != base.NativeRuleMatched ||
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
