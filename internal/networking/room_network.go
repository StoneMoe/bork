package networking

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sort"
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
	return s
}

type roomEndpoint interface {
	Run(context.Context) error
	Snapshot() endpoint.Snapshot
	SnapshotChanges() <-chan struct{}
	ControlPackets() <-chan endpoint.Datagram
	AudioPackets() <-chan endpoint.Datagram
	InteractivePackets() <-chan endpoint.Datagram
	EnqueueControl([]byte, netip.AddrPort) error
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
		network.tracker, network.initializationErr = tracker.New(options.TrackerURLs, trackerHash, trackerIdentity, endpointUDP, logger)
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
	var mappingExpiryTimer *time.Timer
	var mappingExpiry <-chan time.Time
	stopMappingExpiry := func() {
		mappingExpiry = nil
	}
	resetMappingExpiry := func(expiresAt time.Time) {
		delay := time.Until(expiresAt)
		if mappingExpiryTimer == nil {
			mappingExpiryTimer = time.NewTimer(delay)
		} else {
			mappingExpiryTimer.Reset(delay)
		}
		mappingExpiry = mappingExpiryTimer.C
	}
	applyMapping := func(mapping *portmap.Mapping, mappingError string) {
		if mapping == nil {
			currentMapping = nil
			stopMappingExpiry()
		} else {
			copy := *mapping
			currentMapping = &copy
			resetMappingExpiry(copy.ExpiresAt)
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
		stopMappingExpiry()
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
			if state.Mapping != nil && !time.Now().Before(state.Mapping.ExpiresAt) {
				if state.Error == "" {
					state.Error = "port mapping expired"
				}
				state.Mapping = nil
			}
			applyMapping(state.Mapping, state.Error)
		case <-mappingExpiry:
			mappingExpiry = nil
			if currentMapping == nil {
				continue
			}
			if time.Now().Before(currentMapping.ExpiresAt) {
				resetMappingExpiry(currentMapping.ExpiresAt)
				continue
			}
			applyMapping(nil, "port mapping expired")
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
		if candidate.Type != endpoint.CandidateHost || candidate.Family != "ipv4" {
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
	seenEndpoints := make(map[netip.AddrPort]struct{}, tracker.MaxAnnounceCandidates)
	appendCandidate := func(candidate endpoint.Candidate) {
		if len(candidates) == tracker.MaxAnnounceCandidates {
			return
		}
		address, err := netip.ParseAddrPort(candidate.Address)
		if err != nil || !address.Addr().Unmap().Is4() || address.Port() == 0 {
			return
		}
		ip := address.Addr().Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || carrierGradeNATPrefix.Contains(ip) {
			return
		}
		if candidate.Type == endpoint.CandidatePortMapped && !hasServerReflexiveAddress(snapshot, ip) {
			return
		}
		endpointAddress := netip.AddrPortFrom(ip, address.Port())
		if _, exists := seenEndpoints[endpointAddress]; exists {
			return
		}
		seenEndpoints[endpointAddress] = struct{}{}
		candidates = append(candidates, tracker.AnnounceCandidate{Address: ip, Port: address.Port()})
	}
	for _, candidate := range snapshot.Candidates {
		if candidate.Type == endpoint.CandidatePortMapped {
			appendCandidate(candidate)
		}
	}
	reflexive := make([]endpoint.Candidate, 0, len(snapshot.Candidates))
	for _, candidate := range snapshot.Candidates {
		if candidate.Type == endpoint.CandidateServerReflexive {
			reflexive = append(reflexive, candidate)
		}
	}
	rttByServer := make(map[string]int64, len(snapshot.STUN))
	for _, result := range snapshot.STUN {
		rttByServer[result.Server] = result.RTTMillis
	}
	sort.Slice(reflexive, func(i, j int) bool {
		left, right := rttByServer[reflexive[i].Source], rttByServer[reflexive[j].Source]
		if left <= 0 {
			left = 1<<63 - 1
		}
		if right <= 0 {
			right = 1<<63 - 1
		}
		if left != right {
			return left < right
		}
		return reflexive[i].Address < reflexive[j].Address
	})
	for _, candidate := range reflexive {
		appendCandidate(candidate)
	}
	if len(candidates) == 0 {
		if listenAddress, err := netip.ParseAddrPort(snapshot.ListenAddress); err == nil && listenAddress.Port() != 0 {
			candidates = append(candidates, tracker.AnnounceCandidate{Port: listenAddress.Port()})
		}
	}
	return candidates
}

func hasServerReflexiveAddress(snapshot endpoint.Snapshot, address netip.Addr) bool {
	for _, candidate := range snapshot.Candidates {
		if candidate.Type != endpoint.CandidateServerReflexive {
			continue
		}
		reflexive, err := netip.ParseAddrPort(candidate.Address)
		if err == nil && reflexive.Addr().Unmap() == address {
			return true
		}
	}
	return false
}

func withPortMapping(snapshot endpoint.Snapshot, mapping *portmap.Mapping) endpoint.Snapshot {
	if mapping == nil || !mapping.ExternalAddress.IsValid() || mapping.ExternalAddress.Port() == 0 {
		return snapshot
	}
	family := "ipv6"
	if mapping.ExternalAddress.Addr().Unmap().Is4() {
		family = "ipv4"
	}
	snapshot.Candidates = append(snapshot.Candidates[:len(snapshot.Candidates):len(snapshot.Candidates)], endpoint.Candidate{
		Type:    endpoint.CandidatePortMapped,
		Address: mapping.ExternalAddress.String(),
		Family:  family,
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
func (n *RoomNetwork) AudioPackets() <-chan endpoint.Datagram   { return n.endpoint.AudioPackets() }
func (n *RoomNetwork) InteractivePackets() <-chan endpoint.Datagram {
	return n.endpoint.InteractivePackets()
}

// EnqueueControl reports validation and queue admission, not the UDP write result.
func (n *RoomNetwork) EnqueueControl(data []byte, destination netip.AddrPort) error {
	return n.endpoint.EnqueueControl(data, destination)
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
