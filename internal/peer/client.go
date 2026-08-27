package peer

import (
	"context"
	"crypto/cipher"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/media"
	"bork/internal/networking"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

type ClientSnapshot struct {
	Name          string
	ScreenSharing bool
	RemotePeers   []RemotePeerSnapshot
	Transfers     []FileTransferSnapshot
	Connectivity  ConnectivitySnapshot
}

type ConnectivitySnapshot struct {
	DiscoveryHints []DiscoveryHintSnapshot `json:"discoveryHints"`
}

type DiscoveryHintSnapshot struct {
	Address   string `json:"address"`
	Source    string `json:"source"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

type Client struct {
	localPeerID    identity.PeerID
	roomInvite     invite.Invite
	networkOptions networking.Options
	logger         *slog.Logger

	memberStateMu      sync.Mutex
	desiredMemberState memberState
	memberStateUpdates chan struct{}
	screenCommands     chan screenCommand
	screenVideoChunks  chan ScreenVideoChunk
	fileCommands       chan fileCommand
	fileWorkResults    chan fileWorkResult
	loopReady          chan struct{}
	loopDone           chan struct{}
	fileContext        context.Context
	fileWorkers        sync.WaitGroup

	snapshotMu               sync.RWMutex
	snapshot                 ClientSnapshot
	roomNetwork              *networking.RoomNetwork
	networkSnapshot          networking.RoomSnapshot
	admissionKey             [32]byte
	helloProbePacket         []byte
	discoveredAddresses      map[netip.AddrPort]discoveredAddress
	remotePeers              map[identity.PeerID]*RemotePeer
	topologyRevision         uint64
	topology                 map[identity.PeerID]*topologyPeer
	roomDatagramProtector    cipher.AEAD
	voicePacketSequence      uint64
	roomDatagramReceivers    map[roomDatagramStreamKey]*roomDatagramReceiveState
	screenVideoReceivers     map[roomDatagramStreamKey]*screenVideoReceiveState
	screenVideoRetainedBytes int
	fanout                   outboundFanout
	fanoutDirty              bool
	reliablePeerCursor       identity.PeerID
	localMemberState         memberState
	localScreenState         screenState
	screenPacketSequence     uint64
	screenVideoChunkID       uint32
	fileTransfers            map[[16]byte]*fileTransfer

	stateChanges chan struct{}
	started      atomic.Bool
}

func NewClient(roomInvite invite.Invite, networkOptions networking.Options, logger *slog.Logger) (*Client, error) {
	localPeerID, err := identity.New()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	roomDatagramProtector := protocol.NewRoomDatagramCipher(roomInvite.RoomDatagramKey())
	return &Client{
		localPeerID:           localPeerID,
		roomInvite:            roomInvite,
		networkOptions:        networkOptions,
		logger:                logger,
		snapshot:              ClientSnapshot{Name: roomInvite.DisplayName, RemotePeers: []RemotePeerSnapshot{}},
		admissionKey:          roomInvite.AdmissionKey(),
		discoveredAddresses:   make(map[netip.AddrPort]discoveredAddress),
		remotePeers:           make(map[identity.PeerID]*RemotePeer),
		topology:              make(map[identity.PeerID]*topologyPeer),
		stateChanges:          make(chan struct{}, 1),
		memberStateUpdates:    make(chan struct{}, 1),
		screenCommands:        make(chan screenCommand),
		screenVideoChunks:     make(chan ScreenVideoChunk, maxCompletedScreenVideoChunks),
		fileCommands:          make(chan fileCommand),
		fileWorkResults:       make(chan fileWorkResult, 64),
		loopReady:             make(chan struct{}),
		loopDone:              make(chan struct{}),
		localMemberState:      memberState{revision: 1},
		localScreenState:      screenState{revision: 1},
		topologyRevision:      1,
		roomDatagramProtector: roomDatagramProtector,
		roomDatagramReceivers: make(map[roomDatagramStreamKey]*roomDatagramReceiveState),
		screenVideoReceivers:  make(map[roomDatagramStreamKey]*screenVideoReceiveState),
		fileTransfers:         make(map[[16]byte]*fileTransfer),
		fanoutDirty:           true,
	}, nil
}

func (c *Client) StateChanges() <-chan struct{} {
	return c.stateChanges
}

func (c *Client) Ready() <-chan struct{} { return c.loopReady }

func (c *Client) Loop(parent context.Context, mediaPort media.PeerPort) error {
	if !c.started.CompareAndSwap(false, true) {
		return errors.New("peer client has already been started")
	}
	defer close(c.loopDone)
	if err := c.initHelloProbe(); err != nil {
		return err
	}
	c.applyDesiredMemberState()

	ctx, cancel := context.WithCancel(parent)
	networkCtx, stopNetwork := context.WithCancel(context.WithoutCancel(parent))
	c.fileContext = ctx
	defer func() {
		c.stopFileTransfers()
		cancel()
		stopNetwork()
		c.fileWorkers.Wait()
		c.discardFileWorkResults()
	}()
	roomNetwork := networking.NewRoomNetwork(c.roomInvite.RoomTag(), c.roomInvite.TrackerHash(), c.localPeerID, c.networkOptions, c.logger)
	c.roomNetwork = roomNetwork
	close(c.loopReady)
	if mediaPort != nil {
		mediaPort.SetSendInvalidator(roomNetwork.SetRealtimeSendGeneration)
		defer mediaPort.SetSendInvalidator(nil)
	}
	defer func() {
		c.roomNetwork = nil
	}()

	networkResult := make(chan error, 1)
	go func() { networkResult <- roomNetwork.Run(networkCtx) }()
	networkChanges := roomNetwork.StateChanges()
	discoveredPeers := roomNetwork.DiscoveredPeers()
	controlPackets := roomNetwork.ControlPackets()
	reliablePackets := roomNetwork.ReliablePackets()
	voicePackets := roomNetwork.VoicePackets()
	screenPackets := roomNetwork.ScreenPackets()
	probeTicker := time.NewTicker(discoveryProbeInterval)
	pingTicker := time.NewTicker(pingInterval)
	cleanupTicker := time.NewTicker(cleanupInterval)
	reliableTicker := time.NewTicker(reliableInterval)
	var sendReady <-chan struct{}
	if mediaPort != nil {
		sendReady = mediaPort.SendReady()
	}
	defer probeTicker.Stop()
	defer pingTicker.Stop()
	defer cleanupTicker.Stop()
	defer reliableTicker.Stop()

	for {
		realtimeEvents := 0
		for realtimeEvents < maxRealtimeEventsPerTurn {
			handled := false
			select {
			case packet, ok := <-voicePackets:
				if !ok {
					voicePackets = nil
				} else {
					c.handlePacket(packet, mediaPort)
					realtimeEvents++
				}
				handled = true
			case <-sendReady:
				mediaPort.ConsumeSend(c.sendVoiceFrame)
				realtimeEvents++
				handled = true
			default:
			}
			if !handled {
				break
			}
		}
		if realtimeEvents == maxRealtimeEventsPerTurn {
			select {
			case packet, ok := <-controlPackets:
				if !ok {
					controlPackets = nil
				} else {
					c.handlePacket(packet, mediaPort)
				}
			default:
			}
		}
		select {
		case <-c.memberStateUpdates:
			c.applyDesiredMemberState()
		case command := <-c.screenCommands:
			c.handleScreenCommand(command)
		case command := <-c.fileCommands:
			c.handleFileCommand(command)
		case result := <-c.fileWorkResults:
			c.handleFileWorkResult(result)
		case _, ok := <-networkChanges:
			if !ok {
				networkChanges = nil
				continue
			}
			c.applyNetworkSnapshot(roomNetwork.Snapshot())
		case hint, ok := <-discoveredPeers:
			if !ok {
				discoveredPeers = nil
				continue
			}
			c.addDiscoveryHint(hint)
		case packet, ok := <-controlPackets:
			if !ok {
				controlPackets = nil
				continue
			}
			c.handlePacket(packet, mediaPort)
		case packet, ok := <-reliablePackets:
			if !ok {
				reliablePackets = nil
				continue
			}
			c.handlePacket(packet, mediaPort)
		case packet, ok := <-voicePackets:
			if !ok {
				voicePackets = nil
				continue
			}
			c.handlePacket(packet, mediaPort)
		case packet, ok := <-screenPackets:
			if !ok {
				screenPackets = nil
				continue
			}
			if !c.handleScreenPacketBurst(packet, screenPackets, mediaPort) {
				screenPackets = nil
			}
		case <-sendReady:
			mediaPort.ConsumeSend(c.sendVoiceFrame)
		case now := <-probeTicker.C:
			c.sendDiscoveryProbes(now)
			c.probeBridgePaths(now)
			c.queueTopologySnapshots(now, false)
		case <-pingTicker.C:
			c.sendPings()
		case now := <-reliableTicker.C:
			c.queueMemberStates()
			c.queueScreenStates()
			c.sendReliable(now)
		case now := <-cleanupTicker.C:
			c.expireRemotePeers()
			c.expireFileTransfers(now)
			if c.expireDiscoveryHints(now) {
				c.publishStateChange()
			}
			c.expireTopology(now)
			c.expireScreenVideoChunks(now)
		case err := <-networkResult:
			if ctx.Err() != nil {
				return nil
			}
			c.applyNetworkSnapshot(roomNetwork.Snapshot())
			return err
		case <-ctx.Done():
			c.sendLeaves()
			stopNetwork()
			<-networkResult
			return nil
		}
	}
}

func (c *Client) handleScreenPacketBurst(first endpoint.Datagram, packets <-chan endpoint.Datagram, mediaPort media.PeerPort) bool {
	c.handleRoomDatagram(first, mediaPort)
	defer c.flushScreenVideoForwards()
	for range maxRealtimeEventsPerTurn - 1 {
		select {
		case packet, ok := <-packets:
			if !ok {
				return false
			}
			c.handleRoomDatagram(packet, mediaPort)
		default:
			return true
		}
	}
	return true
}

func (c *Client) initHelloProbe() error {
	packet, err := protocol.MarshalHelloProbe(c.admissionKey, c.localPeerID)
	if err != nil {
		return err
	}
	c.helloProbePacket = packet
	return nil
}

func (c *Client) RemotePeerCount() int {
	c.snapshotMu.RLock()
	defer c.snapshotMu.RUnlock()
	return len(c.snapshot.RemotePeers)
}

func (c *Client) StateSnapshot() (ClientSnapshot, networking.RoomSnapshot) {
	c.snapshotMu.RLock()
	defer c.snapshotMu.RUnlock()
	snapshot := c.snapshot
	snapshot.RemotePeers = append([]RemotePeerSnapshot{}, snapshot.RemotePeers...)
	snapshot.Transfers = append([]FileTransferSnapshot{}, snapshot.Transfers...)
	snapshot.Connectivity.DiscoveryHints = append([]DiscoveryHintSnapshot{}, snapshot.Connectivity.DiscoveryHints...)
	return snapshot, c.networkSnapshot.Clone()
}

func (c *Client) EncodedInvite() string { return c.roomInvite.Encode() }

func (c *Client) PeerID() identity.PeerID { return c.localPeerID }

func (c *Client) applyNetworkSnapshot(snapshot networking.RoomSnapshot) {
	if !slices.Equal(c.networkSnapshot.Endpoint.Candidates, snapshot.Endpoint.Candidates) {
		c.markTopologyDirty()
	}
	c.snapshotMu.Lock()
	c.networkSnapshot = snapshot
	c.refreshSnapshotLocked()
	c.snapshotMu.Unlock()
	select {
	case c.stateChanges <- struct{}{}:
	default:
	}
}

func (c *Client) publishStateChange() {
	c.snapshotMu.Lock()
	c.refreshSnapshotLocked()
	c.snapshotMu.Unlock()
	select {
	case c.stateChanges <- struct{}{}:
	default:
	}
}

func (c *Client) refreshSnapshotLocked() {
	c.snapshot = ClientSnapshot{
		Name:          c.roomInvite.DisplayName,
		ScreenSharing: c.localScreenState.active(),
		RemotePeers:   c.remotePeerSnapshots(),
		Transfers:     c.fileTransferSnapshots(),
		Connectivity:  c.connectivitySnapshot(),
	}
}

func (c *Client) connectivitySnapshot() ConnectivitySnapshot {
	discoveryHints := make([]DiscoveryHintSnapshot, 0, len(c.discoveredAddresses))
	for address, remembered := range c.discoveredAddresses {
		expiresAt := ""
		if !remembered.expiresAt.IsZero() {
			expiresAt = remembered.expiresAt.UTC().Format(time.RFC3339)
		}
		discoveryHints = append(discoveryHints, DiscoveryHintSnapshot{
			Address: address.String(), Source: string(remembered.source), ExpiresAt: expiresAt,
		})
	}
	sort.Slice(discoveryHints, func(i, j int) bool {
		if discoveryHints[i].Source != discoveryHints[j].Source {
			return discoveryHints[i].Source < discoveryHints[j].Source
		}
		return discoveryHints[i].Address < discoveryHints[j].Address
	})
	return ConnectivitySnapshot{DiscoveryHints: discoveryHints}
}
