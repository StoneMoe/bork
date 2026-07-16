package networking

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"bork/internal/networking/discovery"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

const (
	defaultDiscoveryRetryInitial = 250 * time.Millisecond
	defaultDiscoveryRetryMax     = 30 * time.Second
	maxDiscoveryErrorLength      = 1024
)

type RoomSnapshot struct {
	Endpoint       endpoint.Snapshot
	NetworkError   string
	DiscoveryError string
}

type roomEndpoint interface {
	Run(context.Context) error
	Snapshot() endpoint.Snapshot
	SnapshotChanges() <-chan struct{}
	ControlPackets() <-chan endpoint.Datagram
	VoicePackets() <-chan endpoint.Datagram
	Send([]byte, netip.AddrPort) error
	SendVoiceBatch(endpoint.VoiceBatch) error
	InvalidateVoice(uint64)
}

type RoomNetwork struct {
	roomTag           [16]byte
	logger            *slog.Logger
	endpoint          roomEndpoint
	discoveryServices []discovery.Service
	discoveryRetry    func(context.Context, time.Duration) bool
	discoveryInitial  time.Duration
	discoveryMax      time.Duration

	mu           sync.RWMutex
	snapshot     RoomSnapshot
	stateChanges chan struct{}
	discovered   chan netip.AddrPort
}

type discoveryEvent struct {
	service int
	err     error
}

func NewRoomNetwork(roomTag [16]byte, options endpoint.Options, logger *slog.Logger) *RoomNetwork {
	if logger == nil {
		logger = slog.Default()
	}
	classifier := func(packet []byte) endpoint.PacketClass {
		packetType, packetRoomTag, err := protocol.ParsePrefix(packet)
		if err != nil || packetRoomTag != roomTag || !protocol.ValidPacketSize(packetType, len(packet)) {
			return endpoint.PacketDrop
		}
		if packetType == protocol.PacketVoice {
			return endpoint.PacketVoice
		}
		if packetType == protocol.PacketHello || packetType == protocol.PacketPing || packetType == protocol.PacketPong {
			return endpoint.PacketControl
		}
		return endpoint.PacketDrop
	}
	return newRoomNetwork(roomTag, endpoint.NewClassified(options, logger, classifier), discovery.DefaultServices(), logger)
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
		discovered:        make(chan netip.AddrPort, 64),
	}
}

func (n *RoomNetwork) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)

	endpointResult := make(chan error, 1)
	go func() { endpointResult <- n.endpoint.Run(ctx) }()
	endpointChanges := n.endpoint.SnapshotChanges()
	var discoveryEvents chan discoveryEvent
	var discoveryDone <-chan struct{}
	discoveryErrors := make([]error, len(n.discoveryServices))
	endpointFinished := false
	finish := func(runErr error) error {
		cancel()
		if !endpointFinished {
			endpointErr := <-endpointResult
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
			n.updateSnapshot(func(current *RoomSnapshot) {
				current.Endpoint = endpointSnapshot
			})
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
					index := index
					service := service
					discoveryGroup.Add(1)
					go func() {
						defer discoveryGroup.Done()
						n.superviseDiscovery(ctx, index, service, listenAddress, discoveryEvents)
					}()
				}
				go func() {
					discoveryGroup.Wait()
					close(done)
				}()
				discoveryDone = done
			}
		case event := <-discoveryEvents:
			discoveryErrors[event.service] = event.err
			currentError := discoveryErrorText(discoveryErrors)
			n.updateSnapshot(func(snapshot *RoomSnapshot) { snapshot.DiscoveryError = currentError })
		case err := <-endpointResult:
			endpointFinished = true
			if ctx.Err() != nil {
				return finish(nil)
			}
			if err != nil {
				n.logger.Warn("room UDP endpoint stopped", "error", err)
				n.updateSnapshot(func(snapshot *RoomSnapshot) { snapshot.NetworkError = err.Error() })
			}
			return finish(err)
		case <-ctx.Done():
			return finish(nil)
		}
	}
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

func discoveryErrorText(serviceErrors []error) string {
	messages := make([]string, 0, len(serviceErrors))
	for _, err := range serviceErrors {
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
	snapshot := n.snapshot
	snapshot.Endpoint.Candidates = append([]endpoint.Candidate{}, snapshot.Endpoint.Candidates...)
	snapshot.Endpoint.STUN = append([]endpoint.STUNResult{}, snapshot.Endpoint.STUN...)
	return snapshot
}

func (n *RoomNetwork) StateChanges() <-chan struct{}            { return n.stateChanges }
func (n *RoomNetwork) DiscoveredPeers() <-chan netip.AddrPort   { return n.discovered }
func (n *RoomNetwork) ControlPackets() <-chan endpoint.Datagram { return n.endpoint.ControlPackets() }
func (n *RoomNetwork) VoicePackets() <-chan endpoint.Datagram   { return n.endpoint.VoicePackets() }
func (n *RoomNetwork) SendControl(data []byte, destination netip.AddrPort) error {
	return n.endpoint.Send(data, destination)
}

// SendVoiceBatch transfers ownership of a complete voice fan-out group.
func (n *RoomNetwork) SendVoiceBatch(batch endpoint.VoiceBatch) error {
	return n.endpoint.SendVoiceBatch(batch)
}

func (n *RoomNetwork) InvalidateVoice(generation uint64) {
	n.endpoint.InvalidateVoice(generation)
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
