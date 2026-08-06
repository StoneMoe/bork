package peer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"sync"
	"time"

	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
	"golang.zx2c4.com/wireguard/tun"
)

const (
	reliableChannelVirtualLAN  = 5
	virtualLANStateVersion     = 1
	virtualLANEnvelopeVersion  = 1
	virtualLANMTU              = 1000
	virtualLANPacketOffset     = 16
	virtualLANStateSize        = 1 + 1 + 8 + 16 + 4
	virtualLANEnvelopeSize     = 1 + 32
	maxVirtualLANEvents        = 64
	maxVirtualLANWrites        = 64
	virtualLANPacketsPerSecond = 200.0
	virtualLANPacketBurst      = 32.0
	virtualLANDatagramDeadline = 50 * time.Millisecond
)

var virtualLANPrefix = netip.MustParsePrefix("100.64.0.0/10")
var virtualLANBroadcast = netip.MustParseAddr("100.127.255.255")

type VirtualLANSnapshot struct {
	Status    string
	Address   string
	Interface string
	Error     string
}

type RemoteVirtualLANSnapshot struct {
	PeerID   string
	Address  string
	Conflict bool
}

type virtualLANState struct {
	generation uint64
	enabled    bool
	streamID   [16]byte
	address    netip.Addr
}

type virtualLANEnvelope struct {
	target [32]byte
	packet []byte
}

type virtualLANCommand struct {
	enable bool
	result chan error
}

type virtualLANDevice interface {
	Read([][]byte, []int, int) (int, error)
	Write([][]byte, int) (int, error)
	Name() (string, error)
	Close() error
	BatchSize() int
}

type virtualLANConfigure func(context.Context, string, netip.Addr, int) (func() error, error)
type virtualLANCreate func(context.Context, string, int) (virtualLANDevice, error)

type virtualLANWorker struct {
	stop        chan struct{}
	stopOnce    sync.Once
	setupCtx    context.Context
	cancelSetup context.CancelFunc
	writes      chan []byte
	done        chan struct{}
}

type virtualLANEvent struct {
	worker        *virtualLANWorker
	setup         bool
	stopped       bool
	device        virtualLANDevice
	interfaceName string
	packet        []byte
	err           error
}

func createVirtualLANDevice(ctx context.Context, name string, mtu int) (virtualLANDevice, error) {
	if err := prepareVirtualLANPlatform(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	device, err := tun.CreateTUN(name, mtu)
	if err != nil {
		return nil, fmt.Errorf("create TUN adapter %q (administrator/CAP_NET_ADMIN and platform driver required): %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		_ = device.Close()
		return nil, err
	}
	return device, nil
}

func deriveVirtualLANAddress(roomTag [16]byte, identity []byte) netip.Addr {
	hash := sha256.New()
	hash.Write([]byte("bork/virtual-lan-address-v1"))
	hash.Write(roomTag[:])
	hash.Write(identity)
	host := binary.BigEndian.Uint32(hash.Sum(nil)[:4]) & ((1 << 22) - 1)
	if host == 0 || host == (1<<22)-1 {
		host = 1
	}
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], 0x64400000|host)
	return netip.AddrFrom4(encoded)
}

func virtualLANInterfaceName(identity []byte) string {
	hash := sha256.Sum256(append([]byte("bork/virtual-lan-interface-v1"), identity...))
	return virtualLANPlatformName(hash)
}

func encodeVirtualLANState(state virtualLANState) ([]byte, error) {
	if state.generation == 0 || (state.enabled && (state.streamID == ([16]byte{}) || !validVirtualLANHost(state.address))) || (!state.enabled && (state.streamID != ([16]byte{}) || state.address.IsValid())) {
		return nil, errors.New("virtual LAN state fields are invalid")
	}
	payload := make([]byte, virtualLANStateSize)
	payload[0] = virtualLANStateVersion
	if state.enabled {
		payload[1] = 1
	}
	binary.BigEndian.PutUint64(payload[2:10], state.generation)
	copy(payload[10:26], state.streamID[:])
	if state.enabled {
		address := state.address.As4()
		copy(payload[26:30], address[:])
	}
	return payload, nil
}

func decodeVirtualLANState(payload []byte) (virtualLANState, error) {
	if len(payload) != virtualLANStateSize || payload[0] != virtualLANStateVersion || payload[1] > 1 {
		return virtualLANState{}, errors.New("virtual LAN state encoding is invalid")
	}
	state := virtualLANState{generation: binary.BigEndian.Uint64(payload[2:10]), enabled: payload[1] == 1}
	copy(state.streamID[:], payload[10:26])
	addressBytes := [4]byte(payload[26:30])
	if addressBytes != ([4]byte{}) {
		state.address = netip.AddrFrom4(addressBytes)
	}
	if state.generation == 0 || (state.enabled && (state.streamID == ([16]byte{}) || !validVirtualLANHost(state.address))) || (!state.enabled && (state.streamID != ([16]byte{}) || state.address.IsValid())) {
		return virtualLANState{}, errors.New("virtual LAN state fields are invalid")
	}
	return state, nil
}

func encodeVirtualLANEnvelope(envelope virtualLANEnvelope) ([]byte, error) {
	if envelope.target == ([32]byte{}) || len(envelope.packet) == 0 || len(envelope.packet) > virtualLANMTU || len(envelope.packet) > protocol.MaxGroupDatagramPayload-virtualLANEnvelopeSize {
		return nil, errors.New("virtual LAN datagram fields are invalid")
	}
	payload := make([]byte, virtualLANEnvelopeSize+len(envelope.packet))
	payload[0] = virtualLANEnvelopeVersion
	copy(payload[1:33], envelope.target[:])
	copy(payload[33:], envelope.packet)
	return payload, nil
}

func decodeVirtualLANEnvelope(payload []byte) (virtualLANEnvelope, error) {
	if len(payload) <= virtualLANEnvelopeSize || len(payload) > virtualLANEnvelopeSize+virtualLANMTU || len(payload) > protocol.MaxGroupDatagramPayload || payload[0] != virtualLANEnvelopeVersion {
		return virtualLANEnvelope{}, errors.New("virtual LAN datagram encoding is invalid")
	}
	envelope := virtualLANEnvelope{packet: payload[33:]}
	copy(envelope.target[:], payload[1:33])
	if envelope.target == ([32]byte{}) {
		return virtualLANEnvelope{}, errors.New("virtual LAN target is zero")
	}
	return envelope, nil
}

func validVirtualLANHost(address netip.Addr) bool {
	address = address.Unmap()
	return address.Is4() && virtualLANPrefix.Contains(address) && address != virtualLANPrefix.Addr() && address != virtualLANBroadcast
}

func validateVirtualLANIPv4(packet []byte) (netip.Addr, netip.Addr, error) {
	if len(packet) < 20 || len(packet) > virtualLANMTU || packet[0]>>4 != 4 {
		return netip.Addr{}, netip.Addr{}, errors.New("virtual LAN packet is not bounded IPv4")
	}
	headerSize := int(packet[0]&15) * 4
	totalSize := int(binary.BigEndian.Uint16(packet[2:4]))
	if headerSize < 20 || headerSize > len(packet) || totalSize != len(packet) || totalSize < headerSize || ipv4Checksum(packet[:headerSize]) != 0xffff {
		return netip.Addr{}, netip.Addr{}, errors.New("virtual LAN IPv4 header is invalid")
	}
	source := netip.AddrFrom4([4]byte(packet[12:16]))
	destination := netip.AddrFrom4([4]byte(packet[16:20]))
	if !validVirtualLANHost(source) || !virtualLANPrefix.Contains(destination) || destination == virtualLANPrefix.Addr() {
		return netip.Addr{}, netip.Addr{}, errors.New("virtual LAN IPv4 addresses are invalid")
	}
	return source, destination, nil
}

func ipv4Checksum(header []byte) uint32 {
	var sum uint32
	for index := 0; index < len(header)-1; index += 2 {
		sum += uint32(binary.BigEndian.Uint16(header[index : index+2]))
	}
	if len(header)%2 != 0 {
		sum += uint32(header[len(header)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return sum
}

func (c *Client) EnableVirtualLAN() error  { return c.requestVirtualLAN(true) }
func (c *Client) DisableVirtualLAN() error { return c.requestVirtualLAN(false) }

func (c *Client) requestVirtualLAN(enable bool) error {
	if !c.started.Load() {
		return errors.New("peer client is not running")
	}
	select {
	case <-c.loopReady:
	case <-c.loopDone:
		return errors.New("peer client is not running")
	}
	command := virtualLANCommand{enable: enable, result: make(chan error, 1)}
	select {
	case c.virtualLANCommands <- command:
	case <-c.loopDone:
		return errors.New("peer client is not running")
	}
	select {
	case err := <-command.result:
		return err
	case <-c.loopDone:
		return errors.New("peer client stopped")
	}
}

func (c *Client) handleVirtualLANCommand(command virtualLANCommand) {
	if c.virtualLANPending != nil {
		if !command.enable && c.virtualLANStatus == "enabling" && c.virtualLANWorker != nil {
			c.virtualLANStatus = "disabling"
			c.virtualLANWorker.requestStop()
			command.result <- nil
			c.publishStateChange()
			return
		}
		command.result <- errors.New("virtual LAN operation is already in progress")
		return
	}
	if command.enable {
		if c.localVirtualLAN.enabled {
			command.result <- nil
			return
		}
		c.virtualLANStatus = "enabling"
		c.virtualLANError = ""
		c.virtualLANPending = command.result
		c.virtualLANWorker = c.startVirtualLANWorker()
		c.publishStateChange()
		return
	}
	if !c.localVirtualLAN.enabled || c.virtualLANWorker == nil {
		command.result <- nil
		return
	}
	c.virtualLANStatus = "disabling"
	c.virtualLANError = ""
	c.virtualLANPending = command.result
	c.virtualLANWorker.requestStop()
	c.publishStateChange()
}

func (c *Client) startVirtualLANWorker() *virtualLANWorker {
	setupCtx, cancelSetup := context.WithCancel(context.Background())
	worker := &virtualLANWorker{stop: make(chan struct{}), setupCtx: setupCtx, cancelSetup: cancelSetup, writes: make(chan []byte, maxVirtualLANWrites), done: make(chan struct{})}
	name := virtualLANInterfaceName(c.localIdentity.PublicKey())
	address := deriveVirtualLANAddress(c.roomTag, c.localIdentity.PublicKey())
	go c.runVirtualLANWorker(worker, name, address)
	return worker
}

func (worker *virtualLANWorker) requestStop() {
	worker.stopOnce.Do(func() {
		worker.cancelSetup()
		close(worker.stop)
	})
}

func (c *Client) runVirtualLANWorker(worker *virtualLANWorker, requestedName string, address netip.Addr) {
	defer close(worker.done)
	defer worker.cancelSetup()
	device, err := c.virtualLANCreate(worker.setupCtx, requestedName, virtualLANMTU)
	if err != nil {
		c.virtualLANEvents <- virtualLANEvent{worker: worker, setup: true, err: err}
		return
	}
	name, err := device.Name()
	if err != nil {
		_ = device.Close()
		c.virtualLANEvents <- virtualLANEvent{worker: worker, setup: true, err: fmt.Errorf("get TUN adapter name: %w", err)}
		return
	}
	cleanup, err := c.virtualLANConfigure(worker.setupCtx, name, address, virtualLANMTU)
	if err != nil {
		_ = device.Close()
		c.virtualLANEvents <- virtualLANEvent{worker: worker, setup: true, err: err}
		return
	}
	select {
	case c.virtualLANEvents <- virtualLANEvent{worker: worker, setup: true, device: device, interfaceName: name}:
	case <-worker.stop:
		_ = cleanup()
		_ = device.Close()
		return
	}
	readerDone := make(chan error, 1)
	go c.readVirtualLAN(worker, device, readerDone)
	var stopErr error
selectLoop:
	for {
		select {
		case <-worker.stop:
			break selectLoop
		case packet := <-worker.writes:
			buffer := make([]byte, virtualLANPacketOffset+len(packet))
			copy(buffer[virtualLANPacketOffset:], packet)
			if _, err = device.Write([][]byte{buffer}, virtualLANPacketOffset); err != nil {
				stopErr = fmt.Errorf("write TUN packet: %w", err)
				break selectLoop
			}
		case err = <-readerDone:
			if err != nil {
				stopErr = fmt.Errorf("read TUN packet: %w", err)
			}
			readerDone = nil
			break selectLoop
		}
	}
	if err = cleanup(); err != nil && stopErr == nil {
		stopErr = err
	}
	if err = device.Close(); err != nil && stopErr == nil {
		stopErr = fmt.Errorf("close TUN adapter: %w", err)
	}
	if readerDone != nil {
		<-readerDone
	}
	c.virtualLANEvents <- virtualLANEvent{worker: worker, stopped: true, err: stopErr}
}

func (c *Client) readVirtualLAN(worker *virtualLANWorker, device virtualLANDevice, done chan<- error) {
	batchSize := max(1, device.BatchSize())
	buffers := make([][]byte, batchSize)
	sizes := make([]int, batchSize)
	for index := range buffers {
		buffers[index] = make([]byte, virtualLANPacketOffset+virtualLANMTU+1)
	}
	for {
		count, err := device.Read(buffers, sizes, virtualLANPacketOffset)
		if err != nil {
			done <- err
			return
		}
		for index := 0; index < count && index < len(buffers); index++ {
			if sizes[index] <= 0 || sizes[index] > virtualLANMTU {
				continue
			}
			event := virtualLANEvent{worker: worker, packet: append([]byte(nil), buffers[index][virtualLANPacketOffset:virtualLANPacketOffset+sizes[index]]...)}
			select {
			case c.virtualLANEvents <- event:
			default:
			}
		}
	}
}

func (c *Client) handleVirtualLANEvent(event virtualLANEvent) {
	if event.worker != c.virtualLANWorker {
		return
	}
	if event.setup {
		cancelled := c.virtualLANStatus == "disabling"
		if cancelled && event.err == nil {
			event.worker.requestStop()
			event.err = context.Canceled
		}
		if event.err != nil {
			c.virtualLANWorker = nil
			if cancelled {
				c.virtualLANStatus = "disabled"
				c.virtualLANError = ""
			} else {
				c.virtualLANStatus = "error"
				c.virtualLANError = event.err.Error()
			}
		} else {
			c.virtualLANInterface = event.interfaceName
			c.localVirtualLAN.enabled = true
			c.localVirtualLAN.address = deriveVirtualLANAddress(c.roomTag, c.localIdentity.PublicKey())
			if _, err := rand.Read(c.localVirtualLAN.streamID[:]); err != nil || c.localVirtualLAN.streamID == ([16]byte{}) {
				if err == nil {
					err = errors.New("random source produced a zero virtual LAN stream")
				}
				c.localVirtualLAN.enabled = false
				c.localVirtualLAN.address = netip.Addr{}
				c.virtualLANStatus = "error"
				c.virtualLANError = fmt.Sprintf("create virtual LAN stream: %v", err)
				c.virtualLANWorker.requestStop()
				event.err = errors.New(c.virtualLANError)
			} else {
				c.bumpVirtualLANGeneration()
				c.virtualLANStatus = "enabled"
				c.virtualLANError = ""
				c.queueVirtualLANStates()
			}
		}
		result := c.virtualLANPending
		c.virtualLANPending = nil
		c.publishStateChange()
		result <- event.err
		return
	}
	if event.stopped {
		wasDisabling := c.virtualLANStatus == "disabling"
		c.virtualLANWorker = nil
		c.virtualLANInterface = ""
		c.localVirtualLAN.enabled = false
		c.localVirtualLAN.address = netip.Addr{}
		c.localVirtualLAN.streamID = [16]byte{}
		c.virtualLANSendSequence = 0
		c.clearVirtualLANReceiveStates()
		c.bumpVirtualLANGeneration()
		c.queueVirtualLANStates()
		if event.err != nil {
			c.virtualLANStatus = "error"
			c.virtualLANError = event.err.Error()
		} else if wasDisabling {
			c.virtualLANStatus = "disabled"
			c.virtualLANError = ""
		} else if c.virtualLANStatus != "error" {
			c.virtualLANStatus = "error"
			c.virtualLANError = "virtual LAN adapter stopped unexpectedly"
		}
		result := c.virtualLANPending
		c.virtualLANPending = nil
		c.publishStateChange()
		if result != nil {
			result <- event.err
		}
		return
	}
	if event.packet != nil && c.localVirtualLAN.enabled {
		c.routeVirtualLANPacket(event.packet)
	}
}

func (c *Client) bumpVirtualLANGeneration() {
	c.localVirtualLAN.generation++
	if c.localVirtualLAN.generation == 0 {
		c.localVirtualLAN.generation = 1
	}
}

func (c *Client) queueVirtualLANStates() {
	payload, err := encodeVirtualLANState(c.localVirtualLAN)
	if err != nil {
		return
	}
	for _, peer := range c.remotePeers {
		session := peer.session
		if session == nil || !session.authenticated || session.reliable == nil || session.virtualLANStateSentGeneration == c.localVirtualLAN.generation {
			continue
		}
		session.reliable.discardOutboundChannel(reliableChannelVirtualLAN)
		if session.reliable.queue(reliableChannelVirtualLAN, false, payload) == nil {
			session.virtualLANStateSentGeneration = c.localVirtualLAN.generation
		}
	}
}

func (c *Client) virtualLANStateReady(session *PeeringSession) bool {
	return session != nil && session.authenticated && session.reliable != nil && session.virtualLANStateSentGeneration == c.localVirtualLAN.generation && !session.reliable.pendingChannel(reliableChannelVirtualLAN)
}

func (c *Client) handleVirtualLANState(sender *RemotePeer, payload []byte) {
	state, err := decodeVirtualLANState(payload)
	if err != nil || sender == nil || sender.session == nil || state.generation <= sender.session.remoteVirtualLAN.generation || (state.enabled && state.address != deriveVirtualLANAddress(c.roomTag, sender.identity.PublicKey())) {
		return
	}
	if sender.session.remoteVirtualLAN.streamID != state.streamID {
		c.removeVirtualLANPeer(sender.identity.PeerID())
	}
	sender.session.remoteVirtualLAN = state
	c.publishStateChange()
}

func (c *Client) routeVirtualLANPacket(packet []byte) {
	source, destination, err := validateVirtualLANIPv4(packet)
	if err != nil || source != c.localVirtualLAN.address || !c.virtualLANAddressUnique(source, c.localIdentity.PeerID()) {
		return
	}
	if destination == virtualLANBroadcast {
		for peerID, peer := range c.remotePeers {
			if peer.session != nil && peer.session.remoteVirtualLAN.enabled && c.virtualLANAddressUnique(peer.session.remoteVirtualLAN.address, peerID) {
				c.sendVirtualLANPacket(peer, packet)
			}
		}
		return
	}
	var target *RemotePeer
	for peerID, peer := range c.remotePeers {
		if peer.session != nil && peer.session.authenticated && peer.session.remoteVirtualLAN.enabled && peer.session.remoteVirtualLAN.address == destination && c.virtualLANAddressUnique(destination, peerID) {
			target = peer
			break
		}
	}
	if target != nil {
		c.sendVirtualLANPacket(target, packet)
	}
}

func (c *Client) virtualLANAddressUnique(address netip.Addr, wantedPeerID string) bool {
	owners := 0
	if c.localVirtualLAN.enabled && c.localVirtualLAN.address == address {
		owners++
	}
	for _, peer := range c.remotePeers {
		if peer.session != nil && peer.session.authenticated && peer.session.remoteVirtualLAN.enabled && peer.session.remoteVirtualLAN.address == address {
			owners++
		}
	}
	if owners != 1 {
		return false
	}
	if wantedPeerID == c.localIdentity.PeerID() {
		return c.localVirtualLAN.address == address
	}
	peer := c.remotePeers[wantedPeerID]
	return peer != nil && peer.session != nil && peer.session.remoteVirtualLAN.address == address
}

func (c *Client) sendVirtualLANPacket(peer *RemotePeer, rawPacket []byte) {
	if c.roomNetwork == nil || peer == nil || !c.localVirtualLAN.enabled || !c.virtualLANStateReady(peer.session) || c.virtualLANSendSequence == math.MaxUint64 {
		return
	}
	payload, err := encodeVirtualLANEnvelope(virtualLANEnvelope{target: rawPeerIdentity(peer.identity), packet: rawPacket})
	if err != nil {
		return
	}
	c.virtualLANSendSequence++
	header := protocol.GroupDatagramHeader{Class: protocol.TrafficCustomRealtime, SenderID: c.groupSenderID, StreamID: c.localVirtualLAN.streamID, Sequence: c.virtualLANSendSequence}
	packet, err := protocol.MarshalGroupDatagram(c.roomTag, header, 0, payload, c.groupProtector, c.localIdentity)
	if err != nil {
		return
	}
	destination := peer.session.path.Address()
	if !peer.session.path.IsDirect() {
		now := time.Now()
		c.refreshFanout(now)
		intermediaryIdentity, identityErr := identityFromRaw(peer.session.path.Intermediary())
		intermediary := c.remotePeers[intermediaryIdentity.PeerID()]
		if identityErr != nil || intermediary == nil || intermediary.session == nil || !intermediary.session.authenticated || !intermediary.session.path.IsDirect() || intermediary.session.path.Address() != destination || !c.fanoutReady(now) || !containsPeerID(c.fanout.assignments[intermediaryIdentity.PeerID()], peer.identity.PeerID()) {
			return
		}
	}
	_ = c.roomNetwork.SendRealtimeBatch(endpoint.RealtimeBatch{Class: protocol.TrafficCustomRealtime, Deadline: time.Now().Add(virtualLANDatagramDeadline), Datagrams: []endpoint.RealtimeDatagram{{Data: packet, Destination: destination}}})
}

func (c *Client) handleVirtualLANDatagram(sender *RemotePeer, header protocol.GroupDatagramHeader, packet endpoint.Datagram) {
	if sender.session.remoteVirtualLAN.streamID != header.StreamID || !sender.session.remoteVirtualLAN.enabled || sender.session.path.Address() != packet.From {
		return
	}
	key := groupStreamKey{sender: header.SenderID, stream: header.StreamID, class: header.Class}
	state := c.groupReceivers[key]
	newState := state == nil
	if state == nil {
		state = &groupReceiveState{}
	}
	if !state.mayAccept(header.Sequence) {
		return
	}
	decoded, err := protocol.ParseGroupDatagram(packet.Data, c.roomTag, header, c.groupProtector)
	if err != nil {
		return
	}
	envelope, err := decodeVirtualLANEnvelope(decoded.Payload)
	if err != nil {
		return
	}
	source, destination, err := validateVirtualLANIPv4(envelope.packet)
	senderID := sender.identity.PeerID()
	if err != nil || source != sender.session.remoteVirtualLAN.address || !c.virtualLANAddressUnique(source, senderID) {
		return
	}
	localID := rawPeerIdentity(c.localIdentity.Identity)
	if envelope.target != localID {
		if !sender.session.path.IsDirect() {
			return
		}
		targetIdentity, identityErr := identityFromRaw(envelope.target)
		target := c.remotePeers[targetIdentity.PeerID()]
		if identityErr != nil || target == nil || target.session == nil || !target.session.authenticated || !target.session.path.IsDirect() || !target.session.remoteVirtualLAN.enabled || !containsPeerID(sender.session.inboundFanout.listeners, targetIdentity.PeerID()) || !c.virtualLANAddressUnique(target.session.remoteVirtualLAN.address, targetIdentity.PeerID()) || (destination != target.session.remoteVirtualLAN.address && destination != virtualLANBroadcast) {
			return
		}
		if !c.acceptVirtualLANDatagram(key, state, newState, header.Sequence, packet.ReceivedAt) {
			return
		}
		c.sendRealtimeToPeers(protocol.TrafficCustomRealtime, packet.Data, []string{targetIdentity.PeerID()}, time.Now().Add(virtualLANDatagramDeadline), 0)
		return
	}
	if !c.localVirtualLAN.enabled || c.virtualLANWorker == nil || (destination != c.localVirtualLAN.address && destination != virtualLANBroadcast) || !c.virtualLANAddressUnique(c.localVirtualLAN.address, c.localIdentity.PeerID()) {
		return
	}
	if !c.acceptVirtualLANDatagram(key, state, newState, header.Sequence, packet.ReceivedAt) {
		return
	}
	select {
	case c.virtualLANWorker.writes <- append([]byte(nil), envelope.packet...):
	default:
	}
}

func (c *Client) acceptVirtualLANDatagram(key groupStreamKey, state *groupReceiveState, newState bool, sequence uint64, receivedAt time.Time) bool {
	if !state.accept(sequence) {
		return false
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	if !state.allowCost(receivedAt, 1, virtualLANPacketsPerSecond, virtualLANPacketBurst) {
		return false
	}
	state.lastSeen = receivedAt
	if newState {
		if len(c.groupReceivers) >= maxGroupReceiveStreams {
			return false
		}
		c.groupReceivers[key] = state
	}
	return true
}

func containsPeerID(peerIDs []string, wanted string) bool {
	for _, peerID := range peerIDs {
		if peerID == wanted {
			return true
		}
	}
	return false
}

func (c *Client) removeVirtualLANPeer(peerID string) {
	peer := c.remotePeers[peerID]
	if peer == nil {
		return
	}
	sender := rawPeerIdentity(peer.identity)
	for key := range c.groupReceivers {
		if key.class == protocol.TrafficCustomRealtime && key.sender == sender {
			delete(c.groupReceivers, key)
		}
	}
}

func (c *Client) clearVirtualLANReceiveStates() {
	for key := range c.groupReceivers {
		if key.class == protocol.TrafficCustomRealtime {
			delete(c.groupReceivers, key)
		}
	}
}

func (c *Client) stopVirtualLAN() {
	worker := c.virtualLANWorker
	if worker == nil {
		return
	}
	worker.requestStop()
	for {
		select {
		case event := <-c.virtualLANEvents:
			c.handleVirtualLANEvent(event)
		case <-worker.done:
			return
		}
	}
}

func (c *Client) virtualLANSnapshot() VirtualLANSnapshot {
	status := c.virtualLANStatus
	if status == "" {
		status = "disabled"
	}
	address := ""
	if c.localVirtualLAN.enabled {
		address = c.localVirtualLAN.address.String()
		if !c.virtualLANAddressUnique(c.localVirtualLAN.address, c.localIdentity.PeerID()) {
			status = "conflict"
			return VirtualLANSnapshot{Status: status, Address: address, Interface: c.virtualLANInterface, Error: "virtual LAN address conflicts with another room member"}
		}
	}
	return VirtualLANSnapshot{Status: status, Address: address, Interface: c.virtualLANInterface, Error: c.virtualLANError}
}

func (c *Client) remoteVirtualLANSnapshots() []RemoteVirtualLANSnapshot {
	snapshots := make([]RemoteVirtualLANSnapshot, 0)
	for peerID, peer := range c.remotePeers {
		if peer.session != nil && peer.session.authenticated && peer.session.remoteVirtualLAN.enabled {
			snapshots = append(snapshots, RemoteVirtualLANSnapshot{PeerID: peerID, Address: peer.session.remoteVirtualLAN.address.String(), Conflict: !c.virtualLANAddressUnique(peer.session.remoteVirtualLAN.address, peerID)})
		}
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].PeerID < snapshots[j].PeerID })
	return snapshots
}
