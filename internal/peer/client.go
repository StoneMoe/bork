package peer

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
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

type roomNetwork interface {
	Run(context.Context) error
	Snapshot() networking.RoomSnapshot
	StateChanges() <-chan struct{}
	DiscoveredPeers() <-chan netip.AddrPort
	ControlPackets() <-chan endpoint.Datagram
	VoicePackets() <-chan endpoint.Datagram
	SendControl([]byte, netip.AddrPort) error
	SendVoiceBatch(endpoint.VoiceBatch) error
	InvalidateVoice(uint64)
}

type roomNetworkFactory func() roomNetwork

type ClientSnapshot struct {
	Name         string
	Phase        string
	LocalAddress string
	RemotePeers  []RemotePeerSnapshot
}

type Client struct {
	localIdentity  *identity.LocalIdentity
	roomInvite     invite.Invite
	logger         *slog.Logger
	networkFactory roomNetworkFactory

	snapshotMu          sync.RWMutex
	snapshot            ClientSnapshot
	roomNetwork         roomNetwork
	networkSnapshot     networking.RoomSnapshot
	roomTag             [16]byte
	admissionKey        [32]byte
	ephemeralPrivateKey *ecdh.PrivateKey
	localHello          protocol.HelloPacket
	helloPacket         []byte
	associations        map[[32]byte]*PeeringSession
	discoveredAddresses map[netip.AddrPort]discoveredAddress
	remotePeers         map[string]*RemotePeer

	stateChanges chan struct{}
	started      atomic.Bool
}

func NewClient(localIdentity *identity.LocalIdentity, roomInvite invite.Invite, networkOptions endpoint.Options, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return newClient(
		localIdentity,
		roomInvite,
		func() roomNetwork { return networking.NewRoomNetwork(roomInvite.RoomTag(), networkOptions, logger) },
		logger,
	)
}

func newClient(localIdentity *identity.LocalIdentity, roomInvite invite.Invite, networkFactory roomNetworkFactory, logger *slog.Logger) *Client {
	return &Client{
		localIdentity:       localIdentity,
		roomInvite:          roomInvite,
		logger:              logger,
		networkFactory:      networkFactory,
		snapshot:            ClientSnapshot{Name: roomInvite.DisplayName, Phase: "gathering", RemotePeers: []RemotePeerSnapshot{}},
		roomTag:             roomInvite.RoomTag(),
		admissionKey:        roomInvite.AdmissionKey(),
		discoveredAddresses: make(map[netip.AddrPort]discoveredAddress),
		remotePeers:         make(map[string]*RemotePeer),
		associations:        make(map[[32]byte]*PeeringSession),
		stateChanges:        make(chan struct{}, 1),
	}
}

func (peerLocal *Client) StateChanges() <-chan struct{} {
	return peerLocal.stateChanges
}

func (peerLocal *Client) Loop(parent context.Context, mediaPort media.PeerPort) error {
	if !peerLocal.started.CompareAndSwap(false, true) {
		return errors.New("peer client has already been started")
	}
	if err := peerLocal.rotateHelloEpoch(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	roomNetwork := peerLocal.networkFactory()
	peerLocal.roomNetwork = roomNetwork
	if mediaPort != nil {
		mediaPort.SetSendInvalidator(roomNetwork.InvalidateVoice)
		defer mediaPort.SetSendInvalidator(nil)
	}
	defer func() {
		peerLocal.roomNetwork = nil
	}()

	networkResult := make(chan error, 1)
	go func() { networkResult <- roomNetwork.Run(ctx) }()
	networkChanges := roomNetwork.StateChanges()
	discoveredPeers := roomNetwork.DiscoveredPeers()
	controlPackets := roomNetwork.ControlPackets()
	voicePackets := roomNetwork.VoicePackets()
	helloTicker := time.NewTicker(helloInterval)
	pingTicker := time.NewTicker(pingInterval)
	cleanupTicker := time.NewTicker(time.Second)
	var sendReady <-chan struct{}
	if mediaPort != nil {
		sendReady = mediaPort.SendReady()
	}
	defer helloTicker.Stop()
	defer pingTicker.Stop()
	defer cleanupTicker.Stop()

	for {
		select {
		case _, ok := <-networkChanges:
			if !ok {
				networkChanges = nil
				continue
			}
			peerLocal.applyNetworkSnapshot(roomNetwork.Snapshot())
		case address := <-discoveredPeers:
			peerLocal.addDiscoveredAddress(address)
			peerLocal.sendHello(address)
		case packet, ok := <-controlPackets:
			if !ok {
				controlPackets = nil
				continue
			}
			peerLocal.handlePacket(packet, mediaPort)
		case packet, ok := <-voicePackets:
			if !ok {
				voicePackets = nil
				continue
			}
			peerLocal.handlePacket(packet, mediaPort)
		case <-sendReady:
			mediaPort.ConsumeSend(peerLocal.sendMedia)
		case <-helloTicker.C:
			peerLocal.sendHellos()
		case <-pingTicker.C:
			peerLocal.sendPings()
		case <-cleanupTicker.C:
			peerLocal.expireRemotePeers()
		case err := <-networkResult:
			if ctx.Err() != nil {
				return nil
			}
			peerLocal.applyNetworkSnapshot(roomNetwork.Snapshot())
			return err
		case <-ctx.Done():
			<-networkResult
			return nil
		}
	}
}

func (peerLocal *Client) rotateHelloEpoch() error {
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
	helloPacket, err := protocol.MarshalHello(peerLocal.roomTag, peerLocal.admissionKey, peerLocal.localIdentity, nonce, publicKey)
	if err != nil {
		return fmt.Errorf("marshal local hello: %w", err)
	}
	localHello, err := protocol.ParseHello(helloPacket, peerLocal.roomTag, peerLocal.admissionKey)
	if err != nil {
		return fmt.Errorf("parse local hello: %w", err)
	}
	peerLocal.ephemeralPrivateKey = privateKey
	peerLocal.helloPacket = helloPacket
	peerLocal.localHello = localHello

	retained := make(map[[32]byte]*PeeringSession, len(peerLocal.remotePeers)*2)
	for _, peerRemote := range peerLocal.remotePeers {
		if peerRemote.peerSess != nil {
			retained[peerRemote.peerSess.transcriptHash] = peerRemote.peerSess
		}
		if peerRemote.candidateSess != nil {
			retained[peerRemote.candidateSess.transcriptHash] = peerRemote.candidateSess
		}
	}
	peerLocal.associations = retained
	return nil
}

func (peerLocal *Client) Snapshot() ClientSnapshot {
	snapshot, _ := peerLocal.StateSnapshot()
	return snapshot
}

func (peerLocal *Client) NetworkSnapshot() networking.RoomSnapshot {
	_, snapshot := peerLocal.StateSnapshot()
	return snapshot
}

func (peerLocal *Client) StateSnapshot() (ClientSnapshot, networking.RoomSnapshot) {
	peerLocal.snapshotMu.RLock()
	defer peerLocal.snapshotMu.RUnlock()
	snapshot := peerLocal.snapshot
	snapshot.RemotePeers = append([]RemotePeerSnapshot{}, snapshot.RemotePeers...)
	networkSnapshot := networking.RoomSnapshot{
		Endpoint: endpoint.Snapshot{
			ListenAddress: peerLocal.networkSnapshot.Endpoint.ListenAddress,
			Candidates:    append([]endpoint.Candidate{}, peerLocal.networkSnapshot.Endpoint.Candidates...),
			STUN:          append([]endpoint.STUNResult{}, peerLocal.networkSnapshot.Endpoint.STUN...),
		},
		NetworkError:   peerLocal.networkSnapshot.NetworkError,
		DiscoveryError: peerLocal.networkSnapshot.DiscoveryError,
	}
	return snapshot, networkSnapshot
}

func (peerLocal *Client) EncodedInvite() string { return peerLocal.roomInvite.Encode() }

func (peerLocal *Client) applyNetworkSnapshot(snapshot networking.RoomSnapshot) {
	peerLocal.snapshotMu.Lock()
	peerLocal.networkSnapshot = snapshot
	peerLocal.refreshSnapshotLocked()
	peerLocal.snapshotMu.Unlock()
	select {
	case peerLocal.stateChanges <- struct{}{}:
	default:
	}
}

func (peerLocal *Client) phase() string {
	for _, peerRemote := range peerLocal.remotePeers {
		if peerRemote.peerSess != nil && peerRemote.peerSess.authenticated {
			return "connected"
		}
	}
	if peerLocal.networkSnapshot.Endpoint.ListenAddress != "" {
		return "discovering"
	}
	return "gathering"
}

func (peerLocal *Client) publishStateChange() {
	peerLocal.snapshotMu.Lock()
	peerLocal.refreshSnapshotLocked()
	peerLocal.snapshotMu.Unlock()
	select {
	case peerLocal.stateChanges <- struct{}{}:
	default:
	}
}

func (peerLocal *Client) refreshSnapshotLocked() {
	peerLocal.snapshot = ClientSnapshot{
		Name:         peerLocal.roomInvite.DisplayName,
		Phase:        peerLocal.phase(),
		LocalAddress: peerLocal.networkSnapshot.Endpoint.ListenAddress,
		RemotePeers:  peerLocal.remotePeerSnapshots(),
	}
}
