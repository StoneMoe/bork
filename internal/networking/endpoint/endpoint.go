package endpoint

import (
	"context"
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
)

const (
	maxDatagramSize      = 2048
	udpReceiveBufferSize = 64 * 1024
	maxSTUNServerResults = 8
	maxSTUNAddresses     = 4
	maxPendingSTUN       = maxSTUNServerResults * maxSTUNAddresses
	maxPeerDatagrams     = 32
	maxVoiceBatches      = 1
	maxVoiceFanout       = 16
	maxVoiceWriteTime    = 20 * time.Millisecond
	controlWriteTimeout  = time.Second
	controlSourceRate    = 64.0
	controlSourceBurst   = 64.0
	controlGlobalRate    = 512.0
	controlGlobalBurst   = 256.0
	maxControlSources    = 256
	voiceSourceRate      = 120.0
	voiceSourceBurst     = 24.0
	voiceIPRate          = 800.0
	voiceIPBurst         = 160.0
	voiceGlobalRate      = 2000.0
	voiceGlobalBurst     = 320.0
	maxVoiceSources      = 256
)

type Datagram struct {
	Data       []byte
	From       netip.AddrPort
	ReceivedAt time.Time
}

type PacketClass byte

const (
	PacketDrop PacketClass = iota
	PacketControl
	PacketVoice
)

type PacketClassifier func([]byte) PacketClass

type VoiceDatagram struct {
	Data        []byte
	Destination netip.AddrPort
}

type VoiceBatch struct {
	Datagrams  []VoiceDatagram
	Deadline   time.Time
	Generation uint64
}

type stunResponse struct {
	message []byte
	from    netip.AddrPort
}

type pendingSTUN struct {
	server netip.AddrPort
	result chan stunResponse
}

type stunProbeResult struct {
	mapped    netip.AddrPort
	rttMillis int64
	err       error
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
	logger  *slog.Logger

	mu       sync.RWMutex
	conn     *net.UDPConn
	snapshot Snapshot
	pending  map[stunTransaction]pendingSTUN
	started  bool
	closed   bool

	classifier      PacketClassifier
	snapshotChanges chan struct{}
	controlPackets  chan Datagram
	voicePackets    chan Datagram
	voiceBatches    chan VoiceBatch
	writeGate       chan struct{}
	voiceMu         sync.Mutex
	voiceGeneration uint64
}

func New(options Options, logger *slog.Logger) *Endpoint {
	return NewClassified(options, logger, nil)
}

func NewClassified(options Options, logger *slog.Logger, classifier PacketClassifier) *Endpoint {
	if logger == nil {
		logger = slog.Default()
	}
	if classifier == nil {
		classifier = func([]byte) PacketClass { return PacketControl }
	}
	return &Endpoint{
		options:         normalizeOptions(options),
		logger:          logger,
		pending:         make(map[stunTransaction]pendingSTUN),
		classifier:      classifier,
		snapshotChanges: make(chan struct{}, 1),
		controlPackets:  make(chan Datagram, maxPeerDatagrams),
		voicePackets:    make(chan Datagram, maxPeerDatagrams),
		voiceBatches:    make(chan VoiceBatch, maxVoiceBatches),
		writeGate:       make(chan struct{}, 1),
	}
}

func (e *Endpoint) SnapshotChanges() <-chan struct{} {
	return e.snapshotChanges
}

func (e *Endpoint) Snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneSnapshot(e.snapshot)
}

func (e *Endpoint) ControlPackets() <-chan Datagram { return e.controlPackets }
func (e *Endpoint) VoicePackets() <-chan Datagram   { return e.voicePackets }

func (e *Endpoint) Send(data []byte, destination netip.AddrPort) error {
	if len(data) == 0 || len(data) > maxDatagramSize {
		return fmt.Errorf("peer datagram must contain 1 to %d bytes", maxDatagramSize)
	}
	if !destination.IsValid() || destination.Port() == 0 {
		return errors.New("peer datagram destination is invalid")
	}
	e.mu.RLock()
	conn := e.conn
	closed := e.closed
	e.mu.RUnlock()
	if conn == nil || closed {
		return errors.New("UDP endpoint is not running")
	}
	return e.writeWithTimeout(conn, data, destination, controlWriteTimeout)
}

func (e *Endpoint) SendVoiceBatch(batch VoiceBatch) error {
	if len(batch.Datagrams) == 0 {
		return errors.New("voice batch is empty")
	}
	if len(batch.Datagrams) > maxVoiceFanout {
		return fmt.Errorf("voice batch exceeds %d destinations", maxVoiceFanout)
	}
	if !batch.Deadline.IsZero() && time.Now().After(batch.Deadline) {
		return errors.New("voice batch deadline has expired")
	}
	for _, packet := range batch.Datagrams {
		if len(packet.Data) == 0 || len(packet.Data) > maxDatagramSize {
			return fmt.Errorf("peer datagram must contain 1 to %d bytes", maxDatagramSize)
		}
		if !packet.Destination.IsValid() || packet.Destination.Port() == 0 {
			return errors.New("peer datagram destination is invalid")
		}
	}
	e.mu.RLock()
	running := e.conn != nil && !e.closed
	e.mu.RUnlock()
	if !running {
		return errors.New("UDP endpoint is not running")
	}
	e.voiceMu.Lock()
	defer e.voiceMu.Unlock()
	if batch.Generation != e.voiceGeneration {
		return errors.New("voice batch generation is stale")
	}
	select {
	case e.voiceBatches <- batch:
	default:
		select {
		case <-e.voiceBatches:
		default:
		}
		select {
		case e.voiceBatches <- batch:
		default:
		}
	}
	return nil
}

func (e *Endpoint) InvalidateVoice(generation uint64) {
	e.voiceMu.Lock()
	e.voiceGeneration = generation
	for {
		select {
		case <-e.voiceBatches:
		default:
			e.voiceMu.Unlock()
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
	candidates, err := hostCandidates(local.Addr(), local.Port())
	if err != nil {
		e.logger.Warn("collect host candidates", "error", err)
	}
	e.updateSnapshot(func(snapshot *Snapshot) {
		snapshot.ListenAddress = local.String()
		snapshot.Candidates = candidates
	})
	e.logger.Info("peer UDP endpoint listening", "address", local.String(), "host_candidates", len(candidates))

	workerDone := make(chan error, 2)
	go func() {
		workerDone <- e.readLoop(conn)
	}()
	go func() {
		workerDone <- e.writeVoiceLoop(ctx, conn)
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
	e.closed = true
	e.conn = nil
	for transaction, pending := range e.pending {
		delete(e.pending, transaction)
		close(pending.result)
	}
	e.mu.Unlock()
	close(e.snapshotChanges)
	close(e.controlPackets)
	close(e.voicePackets)
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
	voiceLimiter := newIngressLimiter(voiceSourceRate, voiceSourceBurst, voiceGlobalRate, voiceGlobalBurst, maxVoiceSources)
	voiceIPLimiter := newIngressLimiter(voiceIPRate, voiceIPBurst, voiceGlobalRate, voiceGlobalBurst, maxVoiceSources)
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
				case pending.result <- stunResponse{message: message, from: remote}:
				default:
				}
				continue
			}
		}
		class := e.classifier(buffer[:count])
		if class == PacketDrop {
			continue
		}
		receivedAt := time.Now()
		switch class {
		case PacketControl:
			controlSource := netip.AddrPortFrom(remote.Addr(), 0)
			if !controlLimiter.allow(controlSource, receivedAt) {
				continue
			}
		case PacketVoice:
			voiceIPSource := netip.AddrPortFrom(remote.Addr(), 0)
			if !voiceIPLimiter.allow(voiceIPSource, receivedAt) || !voiceLimiter.allow(remote, receivedAt) {
				continue
			}
		}
		packetData := append([]byte(nil), buffer[:count]...)
		packet := Datagram{Data: packetData, From: remote, ReceivedAt: receivedAt}
		switch class {
		case PacketControl:
			enqueueFresh(e.controlPackets, packet)
		case PacketVoice:
			enqueueFresh(e.voicePackets, packet)
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

func (e *Endpoint) writeVoiceLoop(ctx context.Context, conn *net.UDPConn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case batch := <-e.voiceBatches:
			if err := e.writeVoiceBatch(ctx, conn, batch); errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
				return err
			}
		}
	}
}

func (e *Endpoint) writeVoiceBatch(ctx context.Context, conn *net.UDPConn, batch VoiceBatch) error {
	writeDeadline := time.Now().Add(maxVoiceWriteTime)
	if !batch.Deadline.IsZero() && batch.Deadline.Before(writeDeadline) {
		writeDeadline = batch.Deadline
	}
	if err := e.acquireWrite(ctx, writeDeadline); err != nil {
		return err
	}
	defer e.releaseWrite()

	e.voiceMu.Lock()
	defer e.voiceMu.Unlock()
	if batch.Generation != e.voiceGeneration || (!batch.Deadline.IsZero() && time.Now().After(batch.Deadline)) {
		return nil
	}
	if err := conn.SetWriteDeadline(writeDeadline); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	for _, packet := range batch.Datagrams {
		if !batch.Deadline.IsZero() && time.Now().After(batch.Deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if _, err := conn.WriteToUDPAddrPort(packet.Data, packet.Destination); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			e.logger.Debug("drop voice datagram after send failure", "destination", packet.Destination, "error", err)
		}
	}
	return nil
}

func (e *Endpoint) writeWithTimeout(conn *net.UDPConn, data []byte, destination netip.AddrPort, timeout time.Duration) error {
	return e.writeWithDeadline(context.Background(), conn, data, destination, time.Now().Add(timeout))
}

func (e *Endpoint) writeWithDeadline(ctx context.Context, conn *net.UDPConn, data []byte, destination netip.AddrPort, deadline time.Time) error {
	if err := e.acquireWrite(ctx, deadline); err != nil {
		return err
	}
	defer e.releaseWrite()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err := conn.WriteToUDPAddrPort(data, destination)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func (e *Endpoint) acquireWrite(ctx context.Context, deadline time.Time) error {
	wait := time.Until(deadline)
	if wait <= 0 {
		return context.DeadlineExceeded
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case e.writeGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

func (e *Endpoint) releaseWrite() {
	<-e.writeGate
}

func enqueueFresh(queue chan Datagram, packet Datagram) {
	select {
	case queue <- packet:
	default:
		select {
		case <-queue:
		default:
		}
		select {
		case queue <- packet:
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
	var wait sync.WaitGroup
	for _, server := range servers {
		server := server
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- e.querySTUN(ctx, server)
		}()
	}
	wait.Wait()
	close(results)

	stunResults := make([]STUNResult, 0, len(servers))
	for result := range results {
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
		hosts := snapshot.Candidates[:0]
		for _, candidate := range snapshot.Candidates {
			if candidate.Type == CandidateHost {
				hosts = append(hosts, candidate)
			}
		}
		snapshot.Candidates = hosts
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
				Type:    CandidateServerReflexive,
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
	conn := e.conn
	e.mu.RUnlock()
	if conn == nil {
		result.Error = "UDP endpoint is not running"
		return result
	}
	probes := make(chan stunProbeResult, len(serverAddresses))
	for _, serverAddress := range serverAddresses {
		serverAddress := serverAddress
		go func() { probes <- e.probeSTUN(queryCtx, conn, serverAddress) }()
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

func (e *Endpoint) probeSTUN(ctx context.Context, conn *net.UDPConn, server netip.AddrPort) stunProbeResult {
	transaction, request, err := newBindingRequest()
	if err != nil {
		return stunProbeResult{err: err}
	}
	response := make(chan stunResponse, 1)
	if err := e.addPending(transaction, pendingSTUN{server: server, result: response}); err != nil {
		return stunProbeResult{err: err}
	}
	defer e.removePending(transaction)
	started := time.Now()
	deadline, _ := ctx.Deadline()
	if err := e.writeWithDeadline(ctx, conn, request, server, deadline); err != nil {
		return stunProbeResult{err: fmt.Errorf("send STUN request: %w", err)}
	}
	select {
	case packet, ok := <-response:
		if !ok {
			return stunProbeResult{err: errors.New("UDP endpoint closed")}
		}
		mapped, err := parseBindingResponse(packet.message, transaction)
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

func (e *Endpoint) updateSnapshot(update func(*Snapshot)) {
	e.mu.Lock()
	update(&e.snapshot)
	e.mu.Unlock()
	select {
	case e.snapshotChanges <- struct{}{}:
	default:
	}
}
