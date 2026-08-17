package networking

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"bork/internal/networking/discovery"
	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
	"bork/internal/networking/portmap"
)

const (
	defaultDiscoveryRetryInitial = 250 * time.Millisecond
	defaultDiscoveryRetryMax     = 30 * time.Second
	maxDiscoveryErrorLength      = 1024
	portMappingCleanupTimeout    = 5 * time.Second
)

var carrierGradeNATPrefix = netip.MustParsePrefix("100.64.0.0/10")

type RoomSnapshot struct {
	Endpoint         endpoint.Snapshot
	Tracker          []tracker.ProviderStatus
	NetworkError     string
	DiscoveryError   string
	PortMappingError string
}

func (s RoomSnapshot) Clone() RoomSnapshot {
	s.Endpoint = s.Endpoint.Clone()
	s.Tracker = append([]tracker.ProviderStatus{}, s.Tracker...)
	for index := range s.Tracker {
		s.Tracker[index] = s.Tracker[index].Clone()
	}
	return s
}

type roomEndpoint interface {
	Run(context.Context) error
	Snapshot() endpoint.Snapshot
	SnapshotChanges() <-chan struct{}
	ControlPackets() <-chan endpoint.Datagram
	ReliablePackets() <-chan endpoint.Datagram
	BridgePackets() <-chan endpoint.Datagram
	AudioPackets() <-chan endpoint.Datagram
	InteractivePackets() <-chan endpoint.Datagram
	EnqueueControl([]byte, netip.AddrPort) error
	WriteControl(context.Context, []byte, netip.AddrPort) error
	EnqueueBackground([]byte, netip.AddrPort) error
	SendRealtimeBatch(endpoint.RealtimeBatch) error
	InvalidateRealtime(uint64)
}

type roomTracker interface {
	UpdateCandidates([]tracker.AnnounceCandidate)
	Run(context.Context, chan<- discovery.Hint) error
	Snapshot() []tracker.ProviderStatus
	StatusChanges() <-chan struct{}
}

type RoomNetwork struct {
	roomTag           [16]byte
	logger            *slog.Logger
	endpoint          roomEndpoint
	discoveryServices []discovery.Service
	discoveryRetry    func(context.Context, time.Duration) bool
	discoveryInitial  time.Duration
	discoveryMax      time.Duration
	tracker           roomTracker
	portMapper        portmap.Mapper
	ipv6PortMapper    portmap.Mapper
	initializationErr error

	mu           sync.RWMutex
	snapshot     RoomSnapshot
	stateChanges chan struct{}
	discovered   chan discovery.Hint
}

type discoveryEvent struct {
	service int
	err     error
}

type portMappingEvent struct {
	lane  int
	state *portmap.State
	err   error
}

type portMappingLane struct {
	family  string
	mapper  portmap.Mapper
	cancel  context.CancelFunc
	started bool
	mapping *portmap.Mapping
	err     string
}

type portMappingLanes struct {
	lanes  [2]portMappingLane
	events chan portMappingEvent
}

func newPortMappingLanes(ipv4, ipv6 portmap.Mapper) *portMappingLanes {
	return &portMappingLanes{
		lanes: [2]portMappingLane{
			{family: "ipv4", mapper: ipv4},
			{family: "ipv6", mapper: ipv6},
		},
		events: make(chan portMappingEvent, 8),
	}
}

func (m *portMappingLanes) startAvailable(ctx context.Context, snapshot endpoint.Snapshot) {
	for index := range m.lanes {
		lane := &m.lanes[index]
		internalPort := portMappingInternalPort(snapshot, lane.family)
		if lane.mapper == nil || lane.started || internalPort == 0 {
			continue
		}
		mapperCtx, cancel := context.WithCancel(ctx)
		lane.cancel = cancel
		lane.started = true
		go runPortMappingLane(mapperCtx, index, lane.mapper, internalPort, m.events)
	}
}

func runPortMappingLane(ctx context.Context, lane int, mapper portmap.Mapper, internalPort uint16, events chan<- portMappingEvent) {
	states := make(chan portmap.State, 4)
	result := make(chan error, 1)
	go func() { result <- mapper.Run(ctx, internalPort, states) }()
	for {
		select {
		case state := <-states:
			events <- portMappingEvent{lane: lane, state: &state}
		case err := <-result:
			events <- portMappingEvent{lane: lane, err: err}
			return
		}
	}
}

func (m *portMappingLanes) apply(event portMappingEvent) error {
	lane := &m.lanes[event.lane]
	if event.state != nil {
		lane.err = event.state.Error
		lane.mapping = event.state.Mapping
		return nil
	}
	lane.cancel = nil
	lane.mapping = nil
	if event.err == nil {
		event.err = errors.New("port mapper stopped unexpectedly")
	}
	lane.err = event.err.Error()
	return event.err
}

func (m *portMappingLanes) project(snapshot endpoint.Snapshot) endpoint.Snapshot {
	return withPortMappings(snapshot, m.lanes[0].mapping, m.lanes[1].mapping)
}

func (m *portMappingLanes) errorText() string {
	errors := make([]string, 0, len(m.lanes))
	for _, lane := range m.lanes {
		if lane.err != "" {
			errors = append(errors, lane.family+": "+lane.err)
		}
	}
	return strings.Join(errors, "; ")
}

func (m *portMappingLanes) stop() error {
	pending := 0
	for index := range m.lanes {
		lane := &m.lanes[index]
		if lane.cancel != nil {
			lane.cancel()
			lane.mapping = nil
			pending++
		}
	}
	if pending == 0 {
		return nil
	}
	timer := time.NewTimer(portMappingCleanupTimeout)
	defer timer.Stop()
	for pending > 0 {
		select {
		case event := <-m.events:
			if event.state != nil {
				continue
			}
			m.lanes[event.lane].cancel = nil
			pending--
		case <-timer.C:
			return errors.New("timed out cleaning up port mappings")
		}
	}
	return nil
}

func NewRoomNetwork(roomTag [16]byte, trackerHash [20]byte, trackerIdentity [32]byte, options Options, logger *slog.Logger) *RoomNetwork {
	if logger == nil {
		logger = slog.Default()
	}
	endpointUDP := endpoint.New(options.Endpoint, roomTag, logger)
	network := newRoomNetwork(roomTag, endpointUDP, discovery.DefaultServices(), logger)
	if len(options.TrackerURLs) > 0 {
		network.tracker, network.initializationErr = tracker.New(options.TrackerURLs, trackerHash, trackerIdentity, logger)
	}
	if options.EnablePortMapping {
		network.portMapper = portmap.NewGateway(logger)
		network.ipv6PortMapper = portmap.NewIPv6PCP(logger)
	}
	return network
}

func newRoomNetwork(roomTag [16]byte, endpointUDP roomEndpoint, discoveryServices []discovery.Service, logger *slog.Logger) *RoomNetwork {
	if logger == nil {
		logger = slog.Default()
	}
	return &RoomNetwork{
		roomTag:           roomTag,
		logger:            logger,
		endpoint:          endpointUDP,
		discoveryServices: discoveryServices,
		discoveryRetry:    waitForDiscoveryRetry,
		discoveryInitial:  defaultDiscoveryRetryInitial,
		discoveryMax:      defaultDiscoveryRetryMax,
		stateChanges:      make(chan struct{}, 1),
		discovered:        make(chan discovery.Hint, 64),
	}
}

func (n *RoomNetwork) Run(parent context.Context) error {
	if n.initializationErr != nil {
		return n.initializationErr
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))

	endpointResult := make(chan error, 1)
	go func() { endpointResult <- n.endpoint.Run(ctx) }()
	endpointChanges := n.endpoint.SnapshotChanges()
	var discoveryEvents chan discoveryEvent
	var discoveryDone <-chan struct{}
	discoveryErrors := make([]error, len(n.discoveryServices))
	var trackerResult chan error
	var trackerChanges <-chan struct{}
	var trackerCancel context.CancelFunc
	var trackerError error
	if n.tracker != nil {
		trackerChanges = n.tracker.StatusChanges()
		n.updateSnapshot(func(snapshot *RoomSnapshot) { snapshot.Tracker = n.tracker.Snapshot() })
	}
	portMappings := newPortMappingLanes(n.portMapper, n.ipv6PortMapper)
	applyPortMappings := func() {
		projectedEndpoint := portMappings.project(n.endpoint.Snapshot())
		n.updateSnapshot(func(snapshot *RoomSnapshot) {
			snapshot.Endpoint = projectedEndpoint
			snapshot.PortMappingError = portMappings.errorText()
		})
		if n.tracker != nil {
			n.tracker.UpdateCandidates(trackerAnnounceCandidates(projectedEndpoint))
		}
	}
	finish := func(runErr error) error {
		hadPortMapping := portMappings.lanes[0].mapping != nil || portMappings.lanes[1].mapping != nil
		if cleanupErr := portMappings.stop(); cleanupErr != nil {
			n.logger.Warn("port mapper cleanup timed out", "error", cleanupErr)
			runErr = errors.Join(runErr, cleanupErr)
		}
		if hadPortMapping {
			projectedEndpoint := portMappings.project(n.endpoint.Snapshot())
			n.updateSnapshot(func(snapshot *RoomSnapshot) {
				snapshot.Endpoint = projectedEndpoint
			})
		}
		if trackerResult != nil {
			trackerCancel()
			select {
			case <-trackerResult:
			case <-time.After(5 * time.Second):
				cleanupErr := errors.New("timed out cleaning up tracker registrations")
				n.logger.Warn("tracker cleanup timed out", "error", cleanupErr)
				runErr = errors.Join(runErr, cleanupErr)
			}
			trackerResult = nil
			trackerCancel = nil
		}
		cancel()
		if endpointResult != nil {
			endpointErr := <-endpointResult
			endpointResult = nil
			if runErr == nil && parent.Err() == nil && endpointErr != nil && !errors.Is(endpointErr, context.Canceled) {
				runErr = endpointErr
			}
		}
		if discoveryDone != nil {
			<-discoveryDone
		}
		return runErr
	}
	for {
		select {
		case _, ok := <-endpointChanges:
			if !ok {
				endpointChanges = nil
				continue
			}
			endpointSnapshot := n.endpoint.Snapshot()
			projectedEndpoint := portMappings.project(endpointSnapshot)
			n.updateSnapshot(func(current *RoomSnapshot) {
				current.Endpoint = projectedEndpoint
			})
			if n.tracker != nil {
				n.tracker.UpdateCandidates(trackerAnnounceCandidates(projectedEndpoint))
				if trackerResult == nil && trackerError == nil && endpointSnapshot.ListenAddress != "" {
					result := make(chan error, 1)
					trackerResult = result
					trackerCtx, stopTracker := context.WithCancel(ctx)
					trackerCancel = stopTracker
					go func() { result <- n.tracker.Run(trackerCtx, n.discovered) }()
				}
			}
			portMappings.startAvailable(ctx, endpointSnapshot)
			if discoveryEvents == nil && endpointSnapshot.ListenAddress != "" {
				listenAddress, err := netip.ParseAddrPort(endpointSnapshot.ListenAddress)
				if err != nil {
					return finish(err)
				}
				if len(n.discoveryServices) == 0 {
					continue
				}
				discoveryEvents = make(chan discoveryEvent, len(n.discoveryServices))
				done := make(chan struct{})
				var discoveryGroup sync.WaitGroup
				for index, service := range n.discoveryServices {
					discoveryGroup.Go(func() {
						n.superviseDiscovery(ctx, index, service, listenAddress, discoveryEvents)
					})
				}
				go func() {
					discoveryGroup.Wait()
					close(done)
				}()
				discoveryDone = done
			}
		case event := <-discoveryEvents:
			discoveryErrors[event.service] = event.err
			currentError := discoveryErrorText(discoveryErrors, trackerError)
			n.updateSnapshot(func(snapshot *RoomSnapshot) { snapshot.DiscoveryError = currentError })
		case err := <-trackerResult:
			trackerResult = nil
			trackerCancel()
			trackerCancel = nil
			if ctx.Err() == nil {
				if err == nil {
					err = errors.New("tracker announcer stopped unexpectedly")
				}
				trackerError = err
				n.logger.Warn("tracker announcer stopped", "error", err)
				currentError := discoveryErrorText(discoveryErrors, trackerError)
				n.updateSnapshot(func(snapshot *RoomSnapshot) { snapshot.DiscoveryError = currentError })
			}
		case _, ok := <-trackerChanges:
			if !ok {
				trackerChanges = nil
				continue
			}
			n.updateSnapshot(func(snapshot *RoomSnapshot) { snapshot.Tracker = n.tracker.Snapshot() })
		case event := <-portMappings.events:
			if err := portMappings.apply(event); err != nil {
				n.logger.Warn("port mapper stopped", "family", portMappings.lanes[event.lane].family, "error", err)
			}
			applyPortMappings()
		case err := <-endpointResult:
			endpointResult = nil
			if ctx.Err() != nil {
				return finish(nil)
			}
			if err != nil {
				n.logger.Warn("room UDP endpoint stopped", "error", err)
				n.updateSnapshot(func(snapshot *RoomSnapshot) { snapshot.NetworkError = err.Error() })
			}
			return finish(err)
		case <-parent.Done():
			return finish(nil)
		}
	}
}

func portMappingInternalPort(snapshot endpoint.Snapshot, family string) uint16 {
	listenAddress, err := netip.ParseAddrPort(snapshot.ListenAddress)
	if err != nil || listenAddress.Port() == 0 || !listenAddress.Addr().IsUnspecified() {
		return 0
	}
	for _, candidate := range snapshot.Candidates {
		if candidate.Type != endpoint.CandidateNIC || candidate.Family != family {
			continue
		}
		address, err := netip.ParseAddrPort(candidate.Address)
		if err == nil && address.Port() == listenAddress.Port() {
			return listenAddress.Port()
		}
	}
	return 0
}

func trackerAnnounceCandidates(snapshot endpoint.Snapshot) []tracker.AnnounceCandidate {
	candidates := make([]tracker.AnnounceCandidate, 0, tracker.MaxAnnounceCandidates)
	appendCandidate := func(candidate endpoint.Candidate) bool {
		if len(candidates) == tracker.MaxAnnounceCandidates {
			return false
		}
		address, valid := trackerCandidateAddress(candidate)
		if !valid {
			return false
		}
		announceCandidate := tracker.AnnounceCandidate{Address: address.Addr(), Port: address.Port()}
		if slices.Contains(candidates, announceCandidate) {
			return false
		}
		// One public IPv6 address is enough; keep the other bounded slots for
		// IPv4 NAT mappings, which can differ between STUN servers.
		if announceCandidate.Address.Is6() && slices.ContainsFunc(candidates, func(existing tracker.AnnounceCandidate) bool {
			return existing.Address.Is6()
		}) {
			return false
		}
		candidates = append(candidates, announceCandidate)
		return true
	}
	appendCandidates := func(candidateType endpoint.CandidateType) {
		for _, candidate := range snapshot.Candidates {
			if candidate.Type == candidateType {
				appendCandidate(candidate)
			}
		}
	}
	appendCandidates(endpoint.CandidatePortMapped)
	appendIPv6 := func(candidateType endpoint.CandidateType) {
		for _, candidate := range snapshot.Candidates {
			if candidate.Type == candidateType && candidate.Family == "ipv6" && appendCandidate(candidate) {
				return
			}
		}
	}
	appendIPv6(endpoint.CandidateSTUN)
	// If STUN found no IPv6 mapping, fall back to a public NIC address.
	appendIPv6(endpoint.CandidateNIC)
	// Endpoint keeps STUN candidates in stable server/family order. Reuse that
	// order so changing probe RTT does not restart tracker registrations.
	appendCandidates(endpoint.CandidateSTUN)
	// Public NICs fill only the slots left after port mapping and STUN.
	appendCandidates(endpoint.CandidateNIC)
	if len(candidates) == 0 {
		// Without a public candidate, let the HTTP tracker use the request source
		// address with the room endpoint's listening port.
		listenAddress, err := netip.ParseAddrPort(snapshot.ListenAddress)
		if err == nil && listenAddress.Port() != 0 {
			candidates = append(candidates, tracker.AnnounceCandidate{Port: listenAddress.Port()})
		}
	}
	return candidates
}

func trackerCandidateAddress(candidate endpoint.Candidate) (netip.AddrPort, bool) {
	address, err := netip.ParseAddrPort(candidate.Address)
	if err != nil || address.Port() == 0 {
		return netip.AddrPort{}, false
	}
	ip := address.Addr().Unmap()
	if !usablePublicTrackerAddress(ip) {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ip, address.Port()), true
}

func usablePublicTrackerAddress(address netip.Addr) bool {
	return address.IsGlobalUnicast() &&
		!address.IsPrivate() &&
		!carrierGradeNATPrefix.Contains(address)
}

func withPortMappings(snapshot endpoint.Snapshot, mappings ...*portmap.Mapping) endpoint.Snapshot {
	snapshot.Candidates = snapshot.Candidates[:len(snapshot.Candidates):len(snapshot.Candidates)]
	for _, mapping := range mappings {
		if mapping == nil || !mapping.ExternalAddress.IsValid() || mapping.ExternalAddress.Port() == 0 {
			continue
		}
		family := "ipv6"
		if mapping.ExternalAddress.Addr().Unmap().Is4() {
			family = "ipv4"
		}
		snapshot.Candidates = append(snapshot.Candidates, endpoint.Candidate{
			Type:    endpoint.CandidatePortMapped,
			Address: mapping.ExternalAddress.String(),
			Family:  family,
			Source:  mapping.Provider,
		})
	}
	return snapshot
}

func (n *RoomNetwork) superviseDiscovery(
	ctx context.Context,
	index int,
	service discovery.Service,
	listenAddress netip.AddrPort,
	events chan<- discoveryEvent,
) {
	delay := n.discoveryInitial
	if delay <= 0 {
		delay = defaultDiscoveryRetryInitial
	}
	maxDelay := n.discoveryMax
	if maxDelay < delay {
		maxDelay = delay
	}
	wait := n.discoveryRetry
	if wait == nil {
		wait = waitForDiscoveryRetry
	}

	for {
		startedAt := time.Now()
		err := service.Run(ctx, n.roomTag, listenAddress, n.discovered)
		if ctx.Err() != nil {
			return
		}
		if time.Since(startedAt) >= maxDelay {
			delay = n.discoveryInitial
			if delay <= 0 {
				delay = defaultDiscoveryRetryInitial
			}
		}
		if err == nil {
			err = errors.New("discovery service stopped unexpectedly")
		}
		n.logger.Warn("room discovery degraded", "service", index, "error", err, "retry_in", delay)
		if !sendDiscoveryEvent(ctx, events, discoveryEvent{service: index, err: err}) || !wait(ctx, delay) {
			return
		}
		if !sendDiscoveryEvent(ctx, events, discoveryEvent{service: index}) {
			return
		}
		delay = nextDiscoveryRetryDelay(delay, maxDelay)
	}
}

func sendDiscoveryEvent(ctx context.Context, events chan<- discoveryEvent, event discoveryEvent) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func waitForDiscoveryRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func nextDiscoveryRetryDelay(delay, maximum time.Duration) time.Duration {
	if delay >= maximum || delay > maximum/2 {
		return maximum
	}
	return delay * 2
}

func discoveryErrorText(serviceErrors []error, additional ...error) string {
	messages := make([]string, 0, len(serviceErrors)+len(additional))
	for _, err := range serviceErrors {
		if err != nil {
			messages = append(messages, err.Error())
		}
	}
	for _, err := range additional {
		if err != nil {
			messages = append(messages, err.Error())
		}
	}
	text := strings.Join(messages, "; ")
	if len(text) <= maxDiscoveryErrorLength {
		return text
	}
	end := maxDiscoveryErrorLength - len("...")
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "..."
}

func (n *RoomNetwork) Snapshot() RoomSnapshot {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.snapshot.Clone()
}

func (n *RoomNetwork) StateChanges() <-chan struct{}            { return n.stateChanges }
func (n *RoomNetwork) DiscoveredPeers() <-chan discovery.Hint   { return n.discovered }
func (n *RoomNetwork) ControlPackets() <-chan endpoint.Datagram { return n.endpoint.ControlPackets() }
func (n *RoomNetwork) ReliablePackets() <-chan endpoint.Datagram {
	return n.endpoint.ReliablePackets()
}
func (n *RoomNetwork) BridgePackets() <-chan endpoint.Datagram { return n.endpoint.BridgePackets() }
func (n *RoomNetwork) AudioPackets() <-chan endpoint.Datagram  { return n.endpoint.AudioPackets() }
func (n *RoomNetwork) InteractivePackets() <-chan endpoint.Datagram {
	return n.endpoint.InteractivePackets()
}

// EnqueueControl reports validation and queue admission, not the UDP write result.
func (n *RoomNetwork) EnqueueControl(data []byte, destination netip.AddrPort) error {
	return n.endpoint.EnqueueControl(data, destination)
}

func (n *RoomNetwork) WriteControl(ctx context.Context, data []byte, destination netip.AddrPort) error {
	return n.endpoint.WriteControl(ctx, data, destination)
}

func (n *RoomNetwork) EnqueueBackground(data []byte, destination netip.AddrPort) error {
	return n.endpoint.EnqueueBackground(data, destination)
}

// SendRealtimeBatch transfers ownership of a complete realtime fan-out group.
func (n *RoomNetwork) SendRealtimeBatch(batch endpoint.RealtimeBatch) error {
	return n.endpoint.SendRealtimeBatch(batch)
}

func (n *RoomNetwork) InvalidateRealtime(generation uint64) {
	n.endpoint.InvalidateRealtime(generation)
}

func (n *RoomNetwork) updateSnapshot(update func(*RoomSnapshot)) {
	n.mu.Lock()
	update(&n.snapshot)
	n.mu.Unlock()
	select {
	case n.stateChanges <- struct{}{}:
	default:
	}
}
