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
	var portMapResult chan error
	var portMapStates <-chan portmap.State
	var portMapCancel context.CancelFunc
	portMapperFinished := false
	var currentMapping *portmap.Mapping
	applyMapping := func(mapping *portmap.Mapping, mappingError string) {
		if mapping == nil {
			currentMapping = nil
		} else {
			copy := *mapping
			currentMapping = &copy
		}
		projectedEndpoint := withPortMapping(n.endpoint.Snapshot(), currentMapping)
		n.updateSnapshot(func(snapshot *RoomSnapshot) {
			snapshot.Endpoint = projectedEndpoint
			snapshot.PortMappingError = mappingError
		})
		if n.tracker != nil {
			n.tracker.UpdateCandidates(trackerAnnounceCandidates(projectedEndpoint))
		}
	}
	finish := func(runErr error) error {
		if portMapCancel != nil && !portMapperFinished {
			portMapCancel()
			cleanupDeadline := time.After(5 * time.Second)
			for !portMapperFinished {
				select {
				case <-portMapStates:
				case <-portMapResult:
					portMapperFinished = true
				case <-cleanupDeadline:
					portMapperFinished = true
					cleanupErr := errors.New("timed out cleaning up port mapping")
					n.logger.Warn("port mapper cleanup timed out", "error", cleanupErr)
					runErr = errors.Join(runErr, cleanupErr)
				}
			}
		}
		if currentMapping != nil {
			currentMapping = nil
			projectedEndpoint := withPortMapping(n.endpoint.Snapshot(), nil)
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
			projectedEndpoint := withPortMapping(endpointSnapshot, currentMapping)
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
			if n.portMapper != nil && portMapResult == nil && !portMapperFinished && endpointSnapshot.ListenAddress != "" {
				if internalPort := portMappingInternalPort(endpointSnapshot); internalPort != 0 {
					states := make(chan portmap.State, 4)
					result := make(chan error, 1)
					portMapStates = states
					portMapResult = result
					mapperCtx, stopMapper := context.WithCancel(ctx)
					portMapCancel = stopMapper
					go func() { result <- n.portMapper.Run(mapperCtx, internalPort, states) }()
				}
			}
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
		case state := <-portMapStates:
			applyMapping(state.Mapping, state.Error)
		case err := <-portMapResult:
			portMapperFinished = true
			portMapResult = nil
			portMapStates = nil
			if ctx.Err() == nil {
				if err == nil {
					err = errors.New("port mapper stopped unexpectedly")
				}
				n.logger.Warn("port mapper stopped", "error", err)
				applyMapping(nil, err.Error())
			}
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

func portMappingInternalPort(snapshot endpoint.Snapshot) uint16 {
	listenAddress, err := netip.ParseAddrPort(snapshot.ListenAddress)
	if err != nil || listenAddress.Port() == 0 || !listenAddress.Addr().IsUnspecified() {
		return 0
	}
	for _, candidate := range snapshot.Candidates {
		if candidate.Type != endpoint.CandidateNIC || candidate.Family != "ipv4" {
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

func withPortMapping(snapshot endpoint.Snapshot, mapping *portmap.Mapping) endpoint.Snapshot {
	if mapping == nil || !mapping.ExternalAddress.IsValid() || mapping.ExternalAddress.Port() == 0 {
		return snapshot
	}
	snapshot.Candidates = append(snapshot.Candidates[:len(snapshot.Candidates):len(snapshot.Candidates)], endpoint.Candidate{
		Type:    endpoint.CandidatePortMapped,
		Address: mapping.ExternalAddress.String(),
		Family:  "ipv4",
		Source:  mapping.Provider,
	})
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
