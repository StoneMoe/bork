package peer

import (
	"context"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
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
	"bork/internal/networking/discovery"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

type roomNetwork interface {
	Run(context.Context) error
	Snapshot() networking.RoomSnapshot
	StateChanges() <-chan struct{}
	DiscoveredPeers() <-chan discovery.Hint
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

type roomNetworkFactory func() roomNetwork

type ClientSnapshot struct {
	Name          string
	Phase         string
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
	localIdentity  *identity.LocalIdentity
	roomInvite     invite.Invite
	logger         *slog.Logger
	networkFactory roomNetworkFactory

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

	snapshotMu                 sync.RWMutex
	snapshot                   ClientSnapshot
	roomNetwork                roomNetwork
	networkSnapshot            networking.RoomSnapshot
	roomTag                    [16]byte
	admissionKey               [32]byte
	ephemeralPrivateKey        *ecdh.PrivateKey
	localHello                 protocol.HelloPacket
	helloPacket                []byte
	discoveredAddresses        map[netip.AddrPort]discoveredAddress
	remotePeers                map[string]*RemotePeer
	topologyGeneration         uint64
	topology                   map[string]*topologyPeer
	roomDatagramSenderID       [32]byte
	audioStreamID              [16]byte
	roomDatagramProtector      cipher.AEAD
	audioSendSequence          uint64
	audioStreamPendingTopology bool
	roomDatagramReceivers      map[roomDatagramStreamKey]*roomDatagramReceiveState
	screenVideoReceivers       map[roomDatagramStreamKey]*screenVideoReceiveState
	screenVideoRetainedBytes   int
	fanout                     outboundFanout
	fanoutDirty                bool
	reliablePeerCursor         string
	localMemberState           memberState
	localScreenState           screenState
	screenMediaSendSequence    uint64
	screenVideoChunkID         uint32
	fileTransfers              map[[16]byte]*fileTransfer

	stateChanges chan struct{}
	started      atomic.Bool
}

func NewClient(roomInvite invite.Invite, networkOptions networking.Options, logger *slog.Logger) (*Client, error) {
	localIdentity, err := identity.New()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return newClient(
		localIdentity,
		roomInvite,
		func() roomNetwork {
			var trackerIdentity [32]byte
			copy(trackerIdentity[:], localIdentity.PublicKey())
			return networking.NewRoomNetwork(roomInvite.RoomTag(), roomInvite.TrackerHash(), trackerIdentity, networkOptions, logger)
		},
		logger,
	), nil
}

func newClient(localIdentity *identity.LocalIdentity, roomInvite invite.Invite, networkFactory roomNetworkFactory, logger *slog.Logger) *Client {
	roomDatagramProtector := protocol.NewRoomDatagramCipher(roomInvite.RoomDatagramKey())
	client := &Client{
		localIdentity:         localIdentity,
		roomInvite:            roomInvite,
		logger:                logger,
		networkFactory:        networkFactory,
		snapshot:              ClientSnapshot{Name: roomInvite.DisplayName, Phase: "gathering", RemotePeers: []RemotePeerSnapshot{}},
		roomTag:               roomInvite.RoomTag(),
		admissionKey:          roomInvite.AdmissionKey(),
		discoveredAddresses:   make(map[netip.AddrPort]discoveredAddress),
		remotePeers:           make(map[string]*RemotePeer),
		topology:              make(map[string]*topologyPeer),
		stateChanges:          make(chan struct{}, 1),
		memberStateUpdates:    make(chan struct{}, 1),
		screenCommands:        make(chan screenCommand),
		screenVideoChunks:     make(chan ScreenVideoChunk, maxCompletedScreenVideoChunks),
		fileCommands:          make(chan fileCommand),
		fileWorkResults:       make(chan fileWorkResult, 64),
		loopReady:             make(chan struct{}),
		loopDone:              make(chan struct{}),
		localMemberState:      memberState{generation: 1},
		localScreenState:      screenState{generation: 1},
		topologyGeneration:    1,
		roomDatagramProtector: roomDatagramProtector,
		roomDatagramReceivers: make(map[roomDatagramStreamKey]*roomDatagramReceiveState),
		screenVideoReceivers:  make(map[roomDatagramStreamKey]*screenVideoReceiveState),
		fileTransfers:         make(map[[16]byte]*fileTransfer),
		fanoutDirty:           true,
	}
	copy(client.roomDatagramSenderID[:], localIdentity.PublicKey())
	return client
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
	if err := c.rotateHelloEpoch(); err != nil {
		return err
	}
	c.initAudioStream()
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
	roomNetwork := c.networkFactory()
	c.roomNetwork = roomNetwork
	close(c.loopReady)
	if mediaPort != nil {
		mediaPort.SetSendInvalidator(roomNetwork.InvalidateRealtime)
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
	bridgePackets := roomNetwork.BridgePackets()
	audioPackets := roomNetwork.AudioPackets()
	interactivePackets := roomNetwork.InteractivePackets()
	helloTicker := time.NewTicker(helloInterval)
	pingTicker := time.NewTicker(pingInterval)
	cleanupTicker := time.NewTicker(cleanupInterval)
	reliableTicker := time.NewTicker(reliableInterval)
	var sendReady <-chan struct{}
	if mediaPort != nil {
		sendReady = mediaPort.SendReady()
	}
	defer helloTicker.Stop()
	defer pingTicker.Stop()
	defer cleanupTicker.Stop()
	defer reliableTicker.Stop()

	for {
		realtimeEvents := 0
		for realtimeEvents < maxRealtimeEventsPerTurn {
			handled := false
			select {
			case packet, ok := <-audioPackets:
				if !ok {
					audioPackets = nil
				} else {
					c.handlePacket(packet, mediaPort)
					realtimeEvents++
				}
				handled = true
			case <-sendReady:
				mediaPort.ConsumeSend(c.sendAudioFrame)
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
		case packet, ok := <-bridgePackets:
			if !ok {
				bridgePackets = nil
				continue
			}
			c.handlePacket(packet, mediaPort)
		case packet, ok := <-audioPackets:
			if !ok {
				audioPackets = nil
				continue
			}
			c.handlePacket(packet, mediaPort)
		case packet, ok := <-interactivePackets:
			if !ok {
				interactivePackets = nil
				continue
			}
			c.handlePacket(packet, mediaPort)
		case <-sendReady:
			mediaPort.ConsumeSend(c.sendAudioFrame)
		case now := <-helloTicker.C:
			c.sendHellos(now)
			c.sendBridgeHellos(now)
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

func (c *Client) rotateHelloEpoch() error {
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate X25519 key: %w", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate handshake nonce: %w", err)
	}
	var publicKey [32]byte
	copy(publicKey[:], privateKey.PublicKey().Bytes())
	helloPacket, err := protocol.MarshalHello(c.roomTag, c.admissionKey, c.localIdentity, nonce, publicKey)
	if err != nil {
		return fmt.Errorf("marshal local hello: %w", err)
	}
	localHello, err := protocol.ParseHello(helloPacket, c.roomTag, c.admissionKey)
	if err != nil {
		return fmt.Errorf("parse local hello: %w", err)
	}
	c.ephemeralPrivateKey = privateKey
	c.helloPacket = helloPacket
	c.localHello = localHello
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

func (c *Client) PeerID() string { return c.localIdentity.PeerID() }

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

func (c *Client) phase() string {
	for _, peer := range c.remotePeers {
		if peer.activeSession != nil && peer.activeSession.authenticated {
			return "connected"
		}
	}
	if c.networkSnapshot.Endpoint.ListenAddress != "" {
		return "discovering"
	}
	return "gathering"
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
		Phase:         c.phase(),
		ScreenSharing: c.localScreenState.active,
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
