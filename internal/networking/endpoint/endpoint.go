package endpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
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
	maxPendingSTUN            = maxSTUNServerResults * 2
	maxPeerDatagrams          = 256
	maxAudioBatches           = 64
	maxInteractiveBatches     = 32
	maxAudioWriteTime         = 20 * time.Millisecond
	maxInteractiveWriteTime   = 50 * time.Millisecond
	maxNonAudioWriteTime      = 2 * time.Millisecond
	controlWriteTimeout       = time.Second
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

type stunProbeFunc func(context.Context, netip.AddrPort) stunProbeResult

type Endpoint struct {
	options Options
	roomTag [16]byte
	logger  *slog.Logger

	mu       sync.RWMutex
	conn     *net.UDPConn
	snapshot Snapshot
	pending  map[stunTransaction]pendingSTUN
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
	case protocol.PacketRoomDatagram:
		header, err := protocol.ParseRoomDatagramHeader(packet, e.roomTag)
		if err != nil {
			return packetDrop
		}
		switch header.Class {
		case protocol.TrafficAudio:
			return packetAudio
		case protocol.TrafficInteractive:
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
	case protocol.TrafficInteractive:
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
		class := e.classifyRoomPacket(buffer[:count])
		if class == packetDrop {
			continue
		}
		receivedAt := time.Now()
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
	case protocol.TrafficInteractive:
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
	stunResults := e.querySTUNServers(ctx, servers)
	for _, result := range stunResults {
		if result.Error != "" {
			e.logger.Debug("STUN probe failed", "server", result.Server, "family", result.Family, "error", result.Error)
		} else {
			e.logger.Info("STUN mapping discovered",
				"server", result.Server,
				"family", result.Family,
				"mapped", result.MappedAddress,
				"rtt_ms", result.RTTMillis,
			)
		}
	}
	e.updateSnapshot(func(snapshot *Snapshot) {
		snapshot.Candidates = mergeSTUNCandidates(snapshot.Candidates, stunResults)
		snapshot.STUN = stunResults
	})
}

func (e *Endpoint) querySTUNServers(ctx context.Context, servers []string) []STUNResult {
	batches := make(chan []STUNResult, len(servers))
	for _, server := range servers {
		go func() { batches <- e.querySTUN(ctx, server) }()
	}
	results := make([]STUNResult, 0, len(servers)*2)
	for range servers {
		results = append(results, (<-batches)...)
	}
	sortSTUNResults(results)
	return results
}

func (e *Endpoint) querySTUN(ctx context.Context, server string) []STUNResult {
	queryCtx, cancel := context.WithTimeout(ctx, e.options.STUNTimeout)
	defer cancel()
	serverAddresses, err := resolveSTUNServer(queryCtx, server)
	if err != nil {
		return []STUNResult{{Server: server, Error: err.Error()}}
	}
	return collectSTUNFamilies(queryCtx, server, serverAddresses, e.probeSTUN)
}

func collectSTUNFamilies(ctx context.Context, server string, addresses []netip.AddrPort, probe stunProbeFunc) []STUNResult {
	completed := make(chan STUNResult, len(addresses))
	for _, address := range addresses {
		go func() {
			family := "ipv6"
			if address.Addr().Is4() {
				family = "ipv4"
			}
			result := STUNResult{Server: server, Family: family}
			received := probe(ctx, address)
			if received.err != nil {
				result.Error = received.err.Error()
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					result.Error = "STUN request timed out"
				}
			} else {
				result.MappedAddress = received.mapped.String()
				result.RTTMillis = received.rttMillis
			}
			completed <- result
		}()
	}
	results := make([]STUNResult, 0, len(addresses))
	for range addresses {
		results = append(results, <-completed)
	}
	sortSTUNResults(results)
	return results
}

func sortSTUNResults(results []STUNResult) {
	sort.Slice(results, func(i, j int) bool {
		left, right := results[i], results[j]
		if left.Server != right.Server {
			return left.Server < right.Server
		}
		return left.Family < right.Family
	})
}

func mergeSTUNCandidates(existing []Candidate, results []STUNResult) []Candidate {
	candidates := existing[:0]
	for _, candidate := range existing {
		if candidate.Type == CandidateNIC {
			candidates = append(candidates, candidate)
		}
	}
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		candidate, valid := stunCandidate(result)
		if !valid {
			continue
		}
		if _, duplicate := seen[candidate.Address]; duplicate {
			continue
		}
		seen[candidate.Address] = struct{}{}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func stunCandidate(result STUNResult) (Candidate, bool) {
	address, err := netip.ParseAddrPort(result.MappedAddress)
	if err != nil {
		return Candidate{}, false
	}
	return Candidate{
		Type: CandidateSTUN, Address: result.MappedAddress,
		Family: addressFamily(address.Addr()), Source: result.Server,
	}, true
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
	// One address per family keeps each server bounded while allowing both IP stacks.
	var ipv4, ipv6 netip.Addr
	for _, address := range addresses {
		address = address.Unmap()
		if address.Is4() && !ipv4.IsValid() {
			ipv4 = address
		} else if address.Is6() && !ipv6.IsValid() {
			ipv6 = address
		}
	}
	resolved := make([]netip.AddrPort, 0, 2)
	if ipv4.IsValid() {
		resolved = append(resolved, netip.AddrPortFrom(ipv4, uint16(parsedPort)))
	}
	if ipv6.IsValid() {
		resolved = append(resolved, netip.AddrPortFrom(ipv6, uint16(parsedPort)))
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

func (e *Endpoint) updateSnapshot(update func(*Snapshot)) {
	e.mu.Lock()
	update(&e.snapshot)
	e.mu.Unlock()
	select {
	case e.snapshotChanges <- struct{}{}:
	default:
	}
}
