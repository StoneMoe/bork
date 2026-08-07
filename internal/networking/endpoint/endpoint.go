package endpoint

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"syscall"
	"time"

	"bork/internal/protocol"
)

const (
	maxDatagramSize = 2048
	// MaxRealtimeBatchDatagrams is an internal memory and amplification safety
	// budget, not a room participant limit.
	MaxRealtimeBatchDatagrams = 256
	maxRealtimeBatchBytes     = MaxRealtimeBatchDatagrams * protocol.MaxDatagramSize
	udpReceiveBufferSize      = 64 * 1024
	maxSTUNServerResults      = 8
	maxSTUNAddresses          = 4
	maxPendingSTUN            = maxSTUNServerResults * maxSTUNAddresses
	maxPendingTracker         = 32
	maxTrackerDatagram        = 2048
	maxPeerDatagrams          = 256
	maxAudioBatches           = 64
	maxInteractiveBatches     = 32
	maxAudioWriteTime         = 20 * time.Millisecond
	maxInteractiveWriteTime   = 50 * time.Millisecond
	maxNonAudioWriteTime      = 2 * time.Millisecond
	controlWriteTimeout       = time.Second
	controlSourceRate         = 64.0
	controlSourceBurst        = 64.0
	controlGlobalRate         = 512.0
	controlGlobalBurst        = 256.0
	maxControlSources         = 256
	reliableSourceRate        = 2048.0
	reliableSourceBurst       = 256.0
	reliableGlobalRate        = 8192.0
	reliableGlobalBurst       = 1024.0
	maxReliableSources        = 256
	bridgeSourceRate          = 2048.0
	bridgeSourceBurst         = 256.0
	bridgeGlobalRate          = 8192.0
	bridgeGlobalBurst         = 1024.0
	maxBridgeSources          = 256
	audioGlobalRate           = 10000.0
	audioGlobalBurst          = 1000.0
	// A forwarder may aggregate audio streams; the peer layer still applies
	// the signed per-stream 120 pps limit after authentication.
	audioIPRate            = 2000.0
	audioIPBurst           = 400.0
	maxAudioSources        = 256
	interactiveSourceRate  = 1000.0
	interactiveSourceBurst = 300.0
	interactiveGlobalRate  = 5000.0
	interactiveGlobalBurst = 1000.0
	maxInteractiveSources  = 256
)

type Datagram struct {
	Data       []byte
	From       netip.AddrPort
	ReceivedAt time.Time
}

type packetClass byte

const (
	packetDrop packetClass = iota
	packetControl
	packetReliable
	packetBridge
	packetAudio
	packetInteractive
)

type RealtimeDatagram struct {
	Data        []byte
	Destination netip.AddrPort
}

type RealtimeBatch struct {
	Class      protocol.TrafficClass
	Datagrams  []RealtimeDatagram
	Deadline   time.Time
	Generation uint64
	writeUntil time.Time
}

type queuedWrite struct {
	data        []byte
	destination netip.AddrPort
	deadline    time.Time
	result      chan error
}

type pendingSTUN struct {
	server netip.AddrPort
	result chan []byte
}

type stunProbeResult struct {
	mapped    netip.AddrPort
	rttMillis int64
	err       error
}

type pendingTracker struct {
	server         netip.AddrPort
	expectedAction uint32
	result         chan []byte
}

type tokenBucket struct {
	tokens    float64
	updatedAt time.Time
}

type ingressSource struct {
	bucket   tokenBucket
	lastSeen time.Time
}

type ingressLimiter struct {
	sourceRate  float64
	sourceBurst float64
	globalRate  float64
	globalBurst float64
	maxSources  int
	global      tokenBucket
	sources     map[netip.AddrPort]ingressSource
}

type Endpoint struct {
	options Options
	roomTag [16]byte
	logger  *slog.Logger

	mu       sync.RWMutex
	conn     *net.UDPConn
	snapshot Snapshot
	pending  map[stunTransaction]pendingSTUN
	trackers map[uint32]pendingTracker
	started  bool
	closed   bool

	snapshotChanges    chan struct{}
	controlPackets     chan Datagram
	reliablePackets    chan Datagram
	bridgePackets      chan Datagram
	audioPackets       chan Datagram
	interactivePackets chan Datagram
	audioBatches       chan RealtimeBatch
	interactiveBatches chan RealtimeBatch
	controlWrites      chan queuedWrite
	backgroundWrites   chan queuedWrite
	realtimeMu         sync.Mutex
	realtimeGeneration uint64
}

func New(options Options, roomTag [16]byte, logger *slog.Logger) *Endpoint {
	if logger == nil {
		logger = slog.Default()
	}
	return &Endpoint{
		options:            normalizeOptions(options),
		roomTag:            roomTag,
		logger:             logger,
		pending:            make(map[stunTransaction]pendingSTUN),
		trackers:           make(map[uint32]pendingTracker),
		snapshotChanges:    make(chan struct{}, 1),
		controlPackets:     make(chan Datagram, maxPeerDatagrams),
		reliablePackets:    make(chan Datagram, maxPeerDatagrams),
		bridgePackets:      make(chan Datagram, maxPeerDatagrams),
		audioPackets:       make(chan Datagram, maxPeerDatagrams),
		interactivePackets: make(chan Datagram, maxPeerDatagrams),
		audioBatches:       make(chan RealtimeBatch, maxAudioBatches),
		interactiveBatches: make(chan RealtimeBatch, maxInteractiveBatches),
		controlWrites:      make(chan queuedWrite, 256),
		backgroundWrites:   make(chan queuedWrite, 32),
	}
}

func (e *Endpoint) classifyRoomPacket(packet []byte) packetClass {
	packetType, roomTag, err := protocol.ParsePrefix(packet)
	if err != nil || roomTag != e.roomTag || !protocol.ValidPacketSize(packetType, len(packet)) {
		return packetDrop
	}
	switch packetType {
	case protocol.PacketGroupDatagram:
		header, err := protocol.ParseGroupDatagramHeader(packet, e.roomTag)
		if err != nil {
			return packetDrop
		}
		switch header.Class {
		case protocol.TrafficAudio:
			return packetAudio
		case protocol.TrafficInteractive, protocol.TrafficCustomRealtime:
			return packetInteractive
		}
	case protocol.PacketHello, protocol.PacketPing, protocol.PacketPong:
		return packetControl
	case protocol.PacketReliable:
		return packetReliable
	case protocol.PacketBridgeControl:
		return packetBridge
	}
	return packetDrop
}

func (e *Endpoint) SnapshotChanges() <-chan struct{} {
	return e.snapshotChanges
}

func (e *Endpoint) Snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapshot.Clone()
}

func (e *Endpoint) ControlPackets() <-chan Datagram     { return e.controlPackets }
func (e *Endpoint) ReliablePackets() <-chan Datagram    { return e.reliablePackets }
func (e *Endpoint) BridgePackets() <-chan Datagram      { return e.bridgePackets }
func (e *Endpoint) AudioPackets() <-chan Datagram       { return e.audioPackets }
func (e *Endpoint) InteractivePackets() <-chan Datagram { return e.interactivePackets }

// EnqueueControl validates and admits a control datagram. It does not wait for
// the kernel write because peer control paths must not inherit socket latency.
func (e *Endpoint) EnqueueControl(data []byte, destination netip.AddrPort) error {
	return e.enqueuePeerDatagram(data, destination, e.controlWrites, "control")
}

// EnqueueBackground admits low-priority peer data without blocking its owner.
func (e *Endpoint) EnqueueBackground(data []byte, destination netip.AddrPort) error {
	return e.enqueuePeerDatagram(data, destination, e.backgroundWrites, "background")
}

func (e *Endpoint) enqueuePeerDatagram(data []byte, destination netip.AddrPort, queue chan<- queuedWrite, lane string) error {
	if len(data) == 0 || len(data) > maxDatagramSize {
		return fmt.Errorf("peer datagram must contain 1 to %d bytes", maxDatagramSize)
	}
	if !destination.IsValid() || destination.Port() == 0 {
		return errors.New("peer datagram destination is invalid")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.conn == nil || e.closed {
		return errors.New("UDP endpoint is not running")
	}
	request := queuedWrite{
		data: append([]byte(nil), data...), destination: destination,
		deadline: time.Now().Add(controlWriteTimeout),
	}
	select {
	case queue <- request:
		return nil
	default:
		return fmt.Errorf("UDP %s queue is full", lane)
	}
}

// ExchangeTracker exchanges one BEP 15 request on the room's shared UDP
// socket. Only the exact tracker source, transaction, and response action can
// satisfy the exchange.
func (e *Endpoint) ExchangeTracker(
	ctx context.Context,
	server netip.AddrPort,
	request []byte,
	expectedAction uint32,
	transaction uint32,
) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("tracker context is required")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("tracker exchange requires a deadline")
	}
	server = netip.AddrPortFrom(server.Addr().Unmap(), server.Port())
	if !server.IsValid() || server.Port() == 0 || server.Addr().IsUnspecified() || server.Addr().IsMulticast() {
		return nil, errors.New("tracker server is invalid")
	}
	if len(request) < 16 || len(request) > maxTrackerDatagram {
		return nil, fmt.Errorf("tracker request must contain 16 to %d bytes", maxTrackerDatagram)
	}
	if transaction == 0 || binary.BigEndian.Uint32(request[8:12]) != expectedAction || binary.BigEndian.Uint32(request[12:16]) != transaction {
		return nil, errors.New("tracker request action or transaction is invalid")
	}

	response := make(chan []byte, 1)
	e.mu.Lock()
	if e.closed || e.conn == nil {
		e.mu.Unlock()
		return nil, errors.New("UDP endpoint is closed")
	}
	if len(e.trackers) >= maxPendingTracker {
		e.mu.Unlock()
		return nil, errors.New("too many pending tracker transactions")
	}
	if _, exists := e.trackers[transaction]; exists {
		e.mu.Unlock()
		return nil, errors.New("duplicate tracker transaction")
	}
	e.trackers[transaction] = pendingTracker{server: server, expectedAction: expectedAction, result: response}
	e.mu.Unlock()
	defer e.removePendingTracker(transaction)

	if err := e.queueWrite(ctx, e.backgroundWrites, request, server); err != nil {
		return nil, fmt.Errorf("send tracker request: %w", err)
	}
	select {
	case received, open := <-response:
		if !open {
			return nil, errors.New("UDP endpoint closed during tracker exchange")
		}
		return received, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *Endpoint) SendRealtimeBatch(batch RealtimeBatch) error {
	if len(batch.Datagrams) == 0 {
		return errors.New("realtime batch is empty")
	}
	if len(batch.Datagrams) > MaxRealtimeBatchDatagrams {
		return fmt.Errorf("realtime batch contains more than %d datagrams", MaxRealtimeBatchDatagrams)
	}
	if !batch.Deadline.IsZero() && time.Now().After(batch.Deadline) {
		return errors.New("realtime batch deadline has expired")
	}
	var batches chan RealtimeBatch
	switch batch.Class {
	case protocol.TrafficAudio:
		batches = e.audioBatches
	case protocol.TrafficInteractive, protocol.TrafficCustomRealtime:
		batches = e.interactiveBatches
	default:
		return errors.New("realtime batch traffic class is invalid")
	}
	totalBytes := 0
	for _, packet := range batch.Datagrams {
		if len(packet.Data) == 0 || len(packet.Data) > maxDatagramSize {
			return fmt.Errorf("peer datagram must contain 1 to %d bytes", maxDatagramSize)
		}
		if len(packet.Data) > maxRealtimeBatchBytes-totalBytes {
			return fmt.Errorf("realtime batch contains more than %d bytes", maxRealtimeBatchBytes)
		}
		totalBytes += len(packet.Data)
		if !packet.Destination.IsValid() || packet.Destination.Port() == 0 {
			return errors.New("peer datagram destination is invalid")
		}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.conn == nil || e.closed {
		return errors.New("UDP endpoint is not running")
	}
	e.realtimeMu.Lock()
	defer e.realtimeMu.Unlock()
	if batch.Generation != 0 && batch.Generation != e.realtimeGeneration {
		return errors.New("realtime batch generation is stale")
	}
	enqueueFresh(batches, batch)
	return nil
}

func (e *Endpoint) InvalidateRealtime(generation uint64) {
	e.realtimeMu.Lock()
	e.realtimeGeneration = generation
	retainRealtimeGeneration(e.audioBatches, generation)
	retainRealtimeGeneration(e.interactiveBatches, generation)
	e.realtimeMu.Unlock()
}

func retainRealtimeGeneration(batches chan RealtimeBatch, generation uint64) {
	retained := make([]RealtimeBatch, 0, len(batches))
	for {
		select {
		case batch := <-batches:
			if batch.Generation == 0 || batch.Generation == generation {
				retained = append(retained, batch)
			}
		default:
			for _, batch := range retained {
				batches <- batch
			}
			return
		}
	}
}

func (e *Endpoint) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, err := listenUDP(e.options.ListenAddress)
	if err != nil {
		return err
	}
	defer conn.Close()

	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errors.New("UDP endpoint has already been started")
	}
	e.started = true
	e.conn = conn
	e.mu.Unlock()

	localAddress := conn.LocalAddr().(*net.UDPAddr)
	localIP, ok := netip.AddrFromSlice(localAddress.IP)
	if !ok {
		return errors.New("UDP endpoint has an invalid local address")
	}
	local := netip.AddrPortFrom(localIP.Unmap(), uint16(localAddress.Port))
	candidates, err := nicCandidates(local.Addr(), local.Port())
	if err != nil {
		e.logger.Warn("collect NIC candidates", "error", err)
	}
	e.updateSnapshot(func(snapshot *Snapshot) {
		snapshot.ListenAddress = local.String()
		snapshot.Candidates = candidates
	})
	e.logger.Info("peer UDP endpoint listening", "address", local.String(), "nic_candidates", len(candidates))

	workerDone := make(chan error, 2)
	go func() {
		workerDone <- e.readLoop(conn)
	}()
	go func() {
		workerDone <- e.writeLoop(ctx, conn)
	}()
	discoveryDone := make(chan struct{})
	go func() {
		defer close(discoveryDone)
		e.discoveryLoop(ctx)
	}()

	var runErr error
	workersFinished := 0
	select {
	case <-ctx.Done():
	case runErr = <-workerDone:
		workersFinished++
		if runErr != nil {
			runErr = fmt.Errorf("run UDP endpoint worker: %w", runErr)
		}
	}
	e.stopAcceptingWrites(conn)
	cancel()
	_ = conn.Close()
	for workersFinished < 2 {
		if err := <-workerDone; runErr == nil && err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			runErr = fmt.Errorf("run UDP endpoint worker: %w", err)
		}
		workersFinished++
	}
	<-discoveryDone
	e.mu.Lock()
	for transaction, pending := range e.pending {
		delete(e.pending, transaction)
		close(pending.result)
	}
	for transaction, pending := range e.trackers {
		delete(e.trackers, transaction)
		close(pending.result)
	}
	e.mu.Unlock()
	close(e.snapshotChanges)
	close(e.controlPackets)
	close(e.reliablePackets)
	close(e.bridgePackets)
	close(e.audioPackets)
	close(e.interactivePackets)
	return runErr
}

func listenUDP(address string) (*net.UDPConn, error) {
	resolved, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve UDP listen address %q: %w", address, err)
	}
	conn, err := net.ListenUDP("udp", resolved)
	if err != nil && (resolved.IP == nil || resolved.IP.IsUnspecified()) && unsupportedAddressFamily(err) {
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: resolved.Port})
	}
	if err != nil {
		return nil, fmt.Errorf("listen on UDP address %q: %w", address, err)
	}
	return conn, nil
}

func unsupportedAddressFamily(err error) bool {
	return errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT)
}

func (e *Endpoint) readLoop(conn *net.UDPConn) error {
	buffer := make([]byte, udpReceiveBufferSize)
	controlLimiter := newIngressLimiter(controlSourceRate, controlSourceBurst, controlGlobalRate, controlGlobalBurst, maxControlSources)
	reliableLimiter := newIngressLimiter(reliableSourceRate, reliableSourceBurst, reliableGlobalRate, reliableGlobalBurst, maxReliableSources)
	bridgeLimiter := newIngressLimiter(bridgeSourceRate, bridgeSourceBurst, bridgeGlobalRate, bridgeGlobalBurst, maxBridgeSources)
	audioIPLimiter := newIngressLimiter(audioIPRate, audioIPBurst, audioGlobalRate, audioGlobalBurst, maxAudioSources)
	interactiveLimiter := newIngressLimiter(interactiveSourceRate, interactiveSourceBurst, interactiveGlobalRate, interactiveGlobalBurst, maxInteractiveSources)
	for {
		count, remote, err := conn.ReadFromUDPAddrPort(buffer)
		if err != nil {
			return err
		}
		if count > maxDatagramSize {
			continue
		}
		remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
		transaction, ok := stunTransactionFromMessage(buffer[:count])
		if ok {
			e.mu.RLock()
			pending, exists := e.pending[transaction]
			e.mu.RUnlock()
			if exists && pending.server == remote {
				message := append([]byte(nil), buffer[:count]...)
				select {
				case pending.result <- message:
				default:
				}
				continue
			}
		}
		if count >= 8 && count <= maxTrackerDatagram {
			action := binary.BigEndian.Uint32(buffer[0:4])
			transaction := binary.BigEndian.Uint32(buffer[4:8])
			e.mu.Lock()
			pending, exists := e.trackers[transaction]
			if exists && pending.server == remote && (action == pending.expectedAction || action == 3) {
				delete(e.trackers, transaction)
			} else {
				exists = false
			}
			e.mu.Unlock()
			if exists {
				pending.result <- append([]byte(nil), buffer[:count]...)
				continue
			}
		}
		class := e.classifyRoomPacket(buffer[:count])
		if class == packetDrop {
			continue
		}
		receivedAt := time.Now()
		switch class {
		case packetControl:
			controlSource := netip.AddrPortFrom(remote.Addr(), 0)
			if !controlLimiter.allow(controlSource, receivedAt) {
				continue
			}
		case packetReliable:
			if !reliableLimiter.allow(remote, receivedAt) {
				continue
			}
		case packetBridge:
			if !bridgeLimiter.allow(remote, receivedAt) {
				continue
			}
		case packetAudio:
			audioIPSource := netip.AddrPortFrom(remote.Addr(), 0)
			if !audioIPLimiter.allow(audioIPSource, receivedAt) {
				continue
			}
		case packetInteractive:
			if !interactiveLimiter.allow(remote, receivedAt) {
				continue
			}
		}
		packetData := append([]byte(nil), buffer[:count]...)
		packet := Datagram{Data: packetData, From: remote, ReceivedAt: receivedAt}
		switch class {
		case packetControl:
			enqueueFresh(e.controlPackets, packet)
		case packetReliable:
			enqueueFresh(e.reliablePackets, packet)
		case packetBridge:
			enqueueFresh(e.bridgePackets, packet)
		case packetAudio:
			enqueueFresh(e.audioPackets, packet)
		case packetInteractive:
			enqueueFresh(e.interactivePackets, packet)
		}
	}
}

func newIngressLimiter(sourceRate, sourceBurst, globalRate, globalBurst float64, maxSources int) *ingressLimiter {
	return &ingressLimiter{
		sourceRate:  sourceRate,
		sourceBurst: sourceBurst,
		globalRate:  globalRate,
		globalBurst: globalBurst,
		maxSources:  maxSources,
		sources:     make(map[netip.AddrPort]ingressSource),
	}
}

func (l *ingressLimiter) allow(source netip.AddrPort, now time.Time) bool {
	l.global.refill(now, l.globalRate, l.globalBurst)
	if l.global.tokens < 1 {
		return false
	}
	entry, exists := l.sources[source]
	if !exists {
		if len(l.sources) >= l.maxSources {
			var oldest netip.AddrPort
			var oldestAt time.Time
			for address, candidate := range l.sources {
				if !oldest.IsValid() || candidate.lastSeen.Before(oldestAt) {
					oldest = address
					oldestAt = candidate.lastSeen
				}
			}
			delete(l.sources, oldest)
		}
	}
	entry.lastSeen = now
	entry.bucket.refill(now, l.sourceRate, l.sourceBurst)
	if entry.bucket.tokens < 1 {
		l.sources[source] = entry
		return false
	}
	entry.bucket.tokens--
	l.global.tokens--
	l.sources[source] = entry
	return true
}

func (b *tokenBucket) refill(now time.Time, rate, burst float64) {
	if b.updatedAt.IsZero() {
		b.tokens = burst
		b.updatedAt = now
	} else if now.After(b.updatedAt) {
		b.tokens = min(burst, b.tokens+now.Sub(b.updatedAt).Seconds()*rate)
		b.updatedAt = now
	}
}

func (e *Endpoint) writeLoop(ctx context.Context, conn *net.UDPConn) error {
	defer func() {
		e.stopAcceptingWrites(conn)
		drainErr := net.ErrClosed
		if ctx.Err() != nil {
			drainErr = ctx.Err()
		}
		drainQueuedWrites(e.controlWrites, drainErr)
		drainQueuedWrites(e.backgroundWrites, drainErr)
	}()

	const (
		audioLane = iota
		controlLane
		interactiveLane
		backgroundLane
		laneCount
	)
	weights := [laneCount]int{8, 2, 2, 1}
	lane, remaining := audioLane, weights[audioLane]
	var pendingRealtime [laneCount]RealtimeBatch
	advance := func() {
		lane = (lane + 1) % laneCount
		remaining = weights[lane]
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		var batch RealtimeBatch
		var request queuedWrite
		selected, ready := lane, false
		for checked := 0; checked < laneCount && !ready; checked++ {
			selected = lane
			switch lane {
			case audioLane:
				if len(pendingRealtime[audioLane].Datagrams) > 0 {
					batch = pendingRealtime[audioLane]
					pendingRealtime[audioLane] = RealtimeBatch{}
					ready = true
				} else {
					select {
					case batch = <-e.audioBatches:
						ready = true
					default:
					}
				}
			case controlLane:
				select {
				case request = <-e.controlWrites:
					ready = true
				default:
				}
			case interactiveLane:
				if len(pendingRealtime[interactiveLane].Datagrams) > 0 {
					batch = pendingRealtime[interactiveLane]
					pendingRealtime[interactiveLane] = RealtimeBatch{}
					ready = true
				} else {
					select {
					case batch = <-e.interactiveBatches:
						ready = true
					default:
					}
				}
			case backgroundLane:
				select {
				case request = <-e.backgroundWrites:
					ready = true
				default:
				}
			}
			if ready {
				remaining--
				if remaining == 0 {
					advance()
				}
			} else {
				advance()
			}
		}

		if !ready {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case batch = <-e.audioBatches:
				selected = audioLane
			case request = <-e.controlWrites:
				selected = controlLane
			case batch = <-e.interactiveBatches:
				selected = interactiveLane
			case request = <-e.backgroundWrites:
				selected = backgroundLane
			}
			lane, remaining = selected, weights[selected]-1
			if remaining == 0 {
				advance()
			}
		}

		var err error
		if selected == audioLane || selected == interactiveLane {
			batch, err = e.writeRealtimeDatagram(ctx, conn, batch)
			if len(batch.Datagrams) > 0 {
				pendingRealtime[selected] = batch
			}
		} else {
			err = writeQueued(conn, request)
		}
		if errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
			return err
		}
	}
}

func (e *Endpoint) stopAcceptingWrites(conn *net.UDPConn) {
	e.mu.Lock()
	e.closed = true
	if e.conn == conn {
		e.conn = nil
	}
	e.mu.Unlock()
}

func (e *Endpoint) writeRealtimeDatagram(ctx context.Context, conn *net.UDPConn, batch RealtimeBatch) (RealtimeBatch, error) {
	if len(batch.Datagrams) == 0 {
		return RealtimeBatch{}, nil
	}
	var writeBudget time.Duration
	switch batch.Class {
	case protocol.TrafficAudio:
		writeBudget = maxAudioWriteTime
	case protocol.TrafficInteractive, protocol.TrafficCustomRealtime:
		writeBudget = min(maxInteractiveWriteTime, maxNonAudioWriteTime)
	default:
		return RealtimeBatch{}, nil
	}

	e.realtimeMu.Lock()
	defer e.realtimeMu.Unlock()
	now := time.Now()
	if (batch.Generation != 0 && batch.Generation != e.realtimeGeneration) || (!batch.Deadline.IsZero() && now.After(batch.Deadline)) {
		return RealtimeBatch{}, nil
	}
	if batch.writeUntil.IsZero() {
		batch.writeUntil = now.Add(writeBudget)
		if !batch.Deadline.IsZero() && batch.Deadline.Before(batch.writeUntil) {
			batch.writeUntil = batch.Deadline
		}
	}
	if now.After(batch.writeUntil) {
		return RealtimeBatch{}, nil
	}
	packet := batch.Datagrams[0]
	batch.Datagrams[0] = RealtimeDatagram{}
	batch.Datagrams = batch.Datagrams[1:]
	if err := conn.SetWriteDeadline(batch.writeUntil); err != nil {
		return batch, err
	}
	defer conn.SetWriteDeadline(time.Time{})
	select {
	case <-ctx.Done():
		return batch, ctx.Err()
	default:
	}
	if _, err := conn.WriteToUDPAddrPort(packet.Data, packet.Destination); err != nil {
		if errors.Is(err, net.ErrClosed) {
			return batch, err
		}
		e.logger.Debug("drop realtime datagram after send failure", "destination", packet.Destination, "error", err)
	}
	return batch, nil
}

func (e *Endpoint) queueWrite(ctx context.Context, lane chan<- queuedWrite, data []byte, destination netip.AddrPort) error {
	if ctx == nil {
		return errors.New("write context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("write context requires a deadline")
	}
	if !deadline.After(time.Now()) {
		return context.DeadlineExceeded
	}
	request := queuedWrite{
		data:        append([]byte(nil), data...),
		destination: destination,
		deadline:    deadline,
		result:      make(chan error, 1),
	}
	select {
	case lane <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func writeQueued(conn *net.UDPConn, request queuedWrite) error {
	now := time.Now()
	if !request.deadline.After(now) {
		reportQueuedWrite(request, context.DeadlineExceeded)
		return nil
	}
	writeDeadline := now.Add(maxNonAudioWriteTime)
	if request.deadline.Before(writeDeadline) {
		writeDeadline = request.deadline
	}
	if err := conn.SetWriteDeadline(writeDeadline); err != nil {
		reportQueuedWrite(request, err)
		return err
	}
	_, err := conn.WriteToUDPAddrPort(request.data, request.destination)
	_ = conn.SetWriteDeadline(time.Time{})
	reportQueuedWrite(request, err)
	return err
}

func reportQueuedWrite(request queuedWrite, err error) {
	select {
	case request.result <- err:
	default:
	}
}

func drainQueuedWrites(lane <-chan queuedWrite, err error) {
	for {
		select {
		case request := <-lane:
			reportQueuedWrite(request, err)
		default:
			return
		}
	}
}

func enqueueFresh[T any](queue chan T, value T) {
	select {
	case queue <- value:
	default:
		select {
		case <-queue:
		default:
		}
		select {
		case queue <- value:
		default:
		}
	}
}

func (e *Endpoint) discoveryLoop(ctx context.Context) {
	e.refreshSTUN(ctx)
	if e.options.STUNRefresh == 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(e.options.STUNRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.refreshSTUN(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (e *Endpoint) refreshSTUN(ctx context.Context) {
	servers := e.options.STUNServers
	if len(servers) > maxSTUNServerResults {
		servers = servers[:maxSTUNServerResults]
	}
	results := make(chan STUNResult, len(servers))
	for _, server := range servers {
		go func() { results <- e.querySTUN(ctx, server) }()
	}

	stunResults := make([]STUNResult, 0, len(servers))
	for range servers {
		result := <-results
		stunResults = append(stunResults, result)
		if result.Error != "" {
			e.logger.Debug("STUN probe failed", "server", result.Server, "error", result.Error)
		} else {
			e.logger.Info("STUN mapping discovered",
				"server", result.Server,
				"mapped", result.MappedAddress,
				"rtt_ms", result.RTTMillis,
			)
		}
	}
	sort.Slice(stunResults, func(i, j int) bool { return stunResults[i].Server < stunResults[j].Server })
	e.updateSnapshot(func(snapshot *Snapshot) {
		nics := snapshot.Candidates[:0]
		for _, candidate := range snapshot.Candidates {
			if candidate.Type == CandidateNIC {
				nics = append(nics, candidate)
			}
		}
		snapshot.Candidates = nics
		seen := make(map[string]struct{})
		for _, result := range stunResults {
			if result.MappedAddress == "" {
				continue
			}
			if _, exists := seen[result.MappedAddress]; exists {
				continue
			}
			seen[result.MappedAddress] = struct{}{}
			address, err := netip.ParseAddrPort(result.MappedAddress)
			if err != nil {
				continue
			}
			snapshot.Candidates = append(snapshot.Candidates, Candidate{
				Type:    CandidateSTUN,
				Address: result.MappedAddress,
				Family:  addressFamily(address.Addr()),
				Source:  result.Server,
			})
		}
		snapshot.STUN = stunResults
	})
}

func (e *Endpoint) querySTUN(ctx context.Context, server string) STUNResult {
	result := STUNResult{Server: server}
	queryCtx, cancel := context.WithTimeout(ctx, e.options.STUNTimeout)
	defer cancel()
	serverAddresses, err := resolveSTUNServer(queryCtx, server)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	e.mu.RLock()
	running := e.conn != nil
	e.mu.RUnlock()
	if !running {
		result.Error = "UDP endpoint is not running"
		return result
	}
	probes := make(chan stunProbeResult, len(serverAddresses))
	for _, serverAddress := range serverAddresses {
		go func() { probes <- e.probeSTUN(queryCtx, serverAddress) }()
	}
	var lastErr error
	for range serverAddresses {
		select {
		case probe := <-probes:
			if probe.err == nil {
				cancel()
				result.MappedAddress = probe.mapped.String()
				result.RTTMillis = probe.rttMillis
				return result
			}
			lastErr = probe.err
		case <-queryCtx.Done():
			if ctx.Err() != nil {
				result.Error = ctx.Err().Error()
			} else {
				result.Error = "STUN request timed out"
			}
			return result
		}
	}
	result.Error = lastErr.Error()
	return result
}

func (e *Endpoint) probeSTUN(ctx context.Context, server netip.AddrPort) stunProbeResult {
	transaction, request, err := newBindingRequest()
	if err != nil {
		return stunProbeResult{err: err}
	}
	response := make(chan []byte, 1)
	if err := e.addPending(transaction, pendingSTUN{server: server, result: response}); err != nil {
		return stunProbeResult{err: err}
	}
	defer e.removePending(transaction)
	started := time.Now()
	if err := e.queueWrite(ctx, e.controlWrites, request, server); err != nil {
		return stunProbeResult{err: fmt.Errorf("send STUN request: %w", err)}
	}
	select {
	case packet, ok := <-response:
		if !ok {
			return stunProbeResult{err: errors.New("UDP endpoint closed")}
		}
		mapped, err := parseBindingResponse(packet, transaction)
		if err != nil {
			return stunProbeResult{err: err}
		}
		return stunProbeResult{mapped: mapped, rttMillis: max(1, time.Since(started).Milliseconds())}
	case <-ctx.Done():
		return stunProbeResult{err: ctx.Err()}
	}
}

func resolveSTUNServer(ctx context.Context, server string) ([]netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(server)
	if err != nil {
		return nil, fmt.Errorf("invalid STUN server %q: %w", server, err)
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve STUN server %q: %w", server, err)
	}
	parsedPort, err := net.LookupPort("udp", port)
	if err != nil {
		return nil, fmt.Errorf("resolve STUN port %q: %w", port, err)
	}
	unique := make([]netip.Addr, 0, len(addresses))
	seen := make(map[netip.Addr]struct{})
	for _, address := range addresses {
		address = address.Unmap()
		if !address.IsValid() || (!address.Is4() && !address.Is6()) {
			continue
		}
		if _, exists := seen[address]; !exists {
			seen[address] = struct{}{}
			unique = append(unique, address)
		}
	}
	resolved := make([]netip.AddrPort, 0, min(len(unique), maxSTUNAddresses))
	for _, wantIPv4 := range []bool{true, false} {
		for _, address := range unique {
			if address.Is4() == wantIPv4 {
				resolved = append(resolved, netip.AddrPortFrom(address, uint16(parsedPort)))
				break
			}
		}
	}
	for _, address := range unique {
		if len(resolved) >= maxSTUNAddresses {
			break
		}
		candidate := netip.AddrPortFrom(address, uint16(parsedPort))
		if !slices.Contains(resolved, candidate) {
			resolved = append(resolved, candidate)
		}
	}
	if len(resolved) > 0 {
		return resolved, nil
	}
	return nil, fmt.Errorf("STUN server %q has no IP addresses", server)
}

func (e *Endpoint) addPending(transaction stunTransaction, pending pendingSTUN) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.conn == nil {
		return errors.New("UDP endpoint is closed")
	}
	if len(e.pending) >= maxPendingSTUN {
		return errors.New("too many pending STUN transactions")
	}
	if _, exists := e.pending[transaction]; exists {
		return errors.New("duplicate STUN transaction")
	}
	e.pending[transaction] = pending
	return nil
}

func (e *Endpoint) removePending(transaction stunTransaction) {
	e.mu.Lock()
	delete(e.pending, transaction)
	e.mu.Unlock()
}

func (e *Endpoint) removePendingTracker(transaction uint32) {
	e.mu.Lock()
	delete(e.trackers, transaction)
	e.mu.Unlock()
}

func (e *Endpoint) updateSnapshot(update func(*Snapshot)) {
	e.mu.Lock()
	update(&e.snapshot)
	e.mu.Unlock()
	select {
	case e.snapshotChanges <- struct{}{}:
	default:
	}
}
