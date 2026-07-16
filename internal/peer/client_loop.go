package peer

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"sort"
	"time"

	"bork/internal/identity"
	"bork/internal/media"
	"bork/internal/networking/endpoint"
	"bork/internal/networking/link"
	"bork/internal/protocol"
)

const (
	helloInterval          = 2 * time.Second
	pingInterval           = 2 * time.Second
	remotePeerTimeout      = 8 * time.Second
	maxDiscoveredAddresses = 64
	maxSessions            = 16
	maxAssociations        = 64
	maxCandidatePaths      = 4
)

type discoveredAddress struct {
	lastSeen time.Time
}

func (peerLocal *Client) addDiscoveredAddress(address netip.AddrPort) {
	peerLocal.rememberDiscoveredAddress(address, time.Now())
}

func (peerLocal *Client) rememberDiscoveredAddress(address netip.AddrPort, now time.Time) {
	if !address.IsValid() || address.Port() == 0 {
		return
	}
	if _, exists := peerLocal.discoveredAddresses[address]; exists {
		peerLocal.discoveredAddresses[address] = discoveredAddress{lastSeen: now}
		return
	}
	if len(peerLocal.discoveredAddresses) >= maxDiscoveredAddresses {
		var oldest netip.AddrPort
		var oldestAt time.Time
		for candidate, discovered := range peerLocal.discoveredAddresses {
			if peerLocal.addressInUse(candidate) {
				continue
			}
			if !oldest.IsValid() || discovered.lastSeen.Before(oldestAt) {
				oldest = candidate
				oldestAt = discovered.lastSeen
			}
		}
		if !oldest.IsValid() {
			return
		}
		delete(peerLocal.discoveredAddresses, oldest)
	}
	peerLocal.discoveredAddresses[address] = discoveredAddress{lastSeen: now}
}

func (peerLocal *Client) addressInUse(address netip.AddrPort) bool {
	for _, peerRemote := range peerLocal.remotePeers {
		for _, session := range []*PeeringSession{peerRemote.peerSess, peerRemote.candidateSess} {
			if session == nil {
				continue
			}
			if session.path.Address() == address {
				return true
			}
			if _, exists := session.candidatePaths[address]; exists {
				return true
			}
		}
	}
	return false
}

func (peerLocal *Client) rememberCandidatePath(session *PeeringSession, path link.Path, now time.Time) {
	address := path.Address()
	if session.candidatePaths == nil {
		session.candidatePaths = make(map[netip.AddrPort]*pathProbe)
	}
	if _, exists := session.candidatePaths[address]; exists {
		return
	}
	if len(session.candidatePaths) >= maxCandidatePaths {
		_, newWasDiscovered := peerLocal.discoveredAddresses[address]
		var victim netip.AddrPort
		var victimAt time.Time
		for candidate, probe := range session.candidatePaths {
			_, candidateWasDiscovered := peerLocal.discoveredAddresses[candidate]
			if candidateWasDiscovered {
				continue
			}
			if !victim.IsValid() || probe.startedAt.Before(victimAt) {
				victim = candidate
				victimAt = probe.startedAt
			}
		}
		if !victim.IsValid() && newWasDiscovered {
			for candidate, probe := range session.candidatePaths {
				if !victim.IsValid() || probe.startedAt.Before(victimAt) {
					victim = candidate
					victimAt = probe.startedAt
				}
			}
		}
		if !victim.IsValid() {
			return
		}
		delete(session.candidatePaths, victim)
	}
	session.candidatePaths[address] = &pathProbe{path: path, startedAt: now}
}

func (peerLocal *Client) sendHello(destination netip.AddrPort) {
	if len(peerLocal.helloPacket) == 0 {
		return
	}
	roomNetwork := peerLocal.roomNetwork
	if roomNetwork != nil {
		_ = roomNetwork.SendControl(peerLocal.helloPacket, destination)
	}
}

func (peerLocal *Client) sendHellos() {
	addresses := make([]netip.AddrPort, 0, len(peerLocal.discoveredAddresses))
	for address := range peerLocal.discoveredAddresses {
		addresses = append(addresses, address)
	}
	for _, address := range addresses {
		peerLocal.sendHello(address)
	}
}

func (peerLocal *Client) handlePacket(packet endpoint.Datagram, mediaPort media.PeerPort) {
	packetType, roomTag, err := protocol.ParsePrefix(packet.Data)
	if err != nil || roomTag != peerLocal.roomTag {
		return
	}
	switch packetType {
	case protocol.PacketHello:
		peerLocal.handleHello(packet)
	case protocol.PacketPing, protocol.PacketPong:
		peerLocal.handleSessionPacket(packet)
	case protocol.PacketVoice:
		peerLocal.handleVoicePacket(packet, mediaPort)
	}
}

func (peerLocal *Client) sendMedia(frame media.SendFrame) {
	if len(frame.Payload) == 0 || len(frame.Payload) > protocol.MaxVoicePayload {
		return
	}
	if !frame.Deadline.IsZero() && time.Now().After(frame.Deadline) {
		return
	}
	roomNetwork := peerLocal.roomNetwork
	batch := endpoint.VoiceBatch{
		Datagrams:  make([]endpoint.VoiceDatagram, 0, len(peerLocal.remotePeers)),
		Deadline:   frame.Deadline,
		Generation: frame.Generation,
	}
	stateChanged := false
	for _, peerRemote := range peerLocal.remotePeers {
		peerSess := peerRemote.peerSess
		if peerSess == nil || !peerSess.authenticated {
			continue
		}
		sequence, err := peerSess.media.NextSendSequence()
		if err != nil {
			if peerSess.authenticated {
				peerSess.authenticated = false
				stateChanged = true
			}
			continue
		}
		packet, err := protocol.MarshalVoice(peerLocal.roomTag, peerSess.sessionID, sequence, frame.Timestamp, frame.Payload, peerSess.ciphers.VoiceSend)
		if err != nil {
			continue
		}
		batch.Datagrams = append(batch.Datagrams, endpoint.VoiceDatagram{Data: packet, Destination: peerSess.path.Address()})
	}
	if roomNetwork != nil && len(batch.Datagrams) > 0 {
		_ = roomNetwork.SendVoiceBatch(batch)
	}
	if stateChanged {
		peerLocal.publishStateChange()
	}
}

func (peerLocal *Client) handleVoicePacket(packet endpoint.Datagram, mediaPort media.PeerPort) {
	header, err := protocol.ParseEstablishedHeader(packet.Data)
	if err != nil || header.Type != protocol.PacketVoice || header.RoomTag != peerLocal.roomTag {
		return
	}
	peerSess := peerLocal.sessionForVoiceHeader(header, packet.From)
	if peerSess == nil || !peerSess.authenticated {
		return
	}
	peerRemote := peerLocal.remotePeerForSession(peerSess)
	if peerRemote == nil || !peerSess.media.MayReceive(header.Sequence) {
		return
	}
	decoded, err := protocol.ParseVoice(packet.Data, peerLocal.roomTag, peerSess.sessionID, peerSess.ciphers.VoiceRecv)
	if err != nil || !peerSess.media.CommitReceived(decoded.Sequence) {
		return
	}
	receivedAt := packet.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	if !peerSess.media.AllowReceived(receivedAt) {
		return
	}
	peerSess.lastAuthenticatedPacketAt = time.Now()
	peerRemoteID := peerRemote.identity.PeerID()
	if mediaPort == nil {
		return
	}
	mediaPort.SubmitReceived(media.ReceivedFrame{
		SourceID:   peerRemoteID,
		StreamID:   peerSess.sessionID,
		Sequence:   decoded.Sequence,
		Timestamp:  decoded.Timestamp,
		Payload:    decoded.Payload,
		ReceivedAt: receivedAt,
	})
}

func (peerLocal *Client) handleHello(packet endpoint.Datagram) {
	hello, err := protocol.ParseHello(packet.Data, peerLocal.roomTag, peerLocal.admissionKey)
	if err != nil {
		return
	}
	peerRemoteIdentity, err := identity.FromPublicKey(hello.IdentityKey)
	if err != nil || peerRemoteIdentity.PeerID() == peerLocal.localIdentity.PeerID() {
		return
	}
	path, err := link.NewPath(packet.From)
	if err != nil {
		return
	}
	peerRemoteID := peerRemoteIdentity.PeerID()
	material, err := protocol.DeriveSession(peerLocal.ephemeralPrivateKey, peerLocal.localHello, hello)
	if err != nil {
		return
	}

	peerRemote := peerLocal.remotePeers[peerRemoteID]
	if peerRemote == nil {
		if len(peerLocal.remotePeers) >= maxSessions && !peerLocal.evictInactiveRemotePeer() {
			return
		}
		peerRemote = &RemotePeer{identity: peerRemoteIdentity}
		peerLocal.remotePeers[peerRemoteID] = peerRemote
	}
	var activeSession *PeeringSession
	activeSession = peerRemote.peerSess
	if activeSession != nil && activeSession.transcriptHash == material.TranscriptHash {
		if activeSession.path.Address() != packet.From {
			peerLocal.rememberCandidatePath(activeSession, path, time.Now())
		}
		peerLocal.sendPing(peerRemoteID, false)
		return
	}

	candidateSession := peerLocal.associations[material.TranscriptHash]
	created := candidateSession == nil
	if created {
		if len(peerLocal.associations) >= maxAssociations {
			if err := peerLocal.rotateHelloEpoch(); err != nil {
				return
			}
			material, err = protocol.DeriveSession(peerLocal.ephemeralPrivateKey, peerLocal.localHello, hello)
			if err != nil {
				return
			}
			candidateSession = peerLocal.associations[material.TranscriptHash]
			created = candidateSession == nil
		}
	}
	attached := peerRemote.candidateSess != candidateSession
	if created {
		candidateSession, err = newPeeringSession(path, material, time.Now())
		if err != nil {
			return
		}
		peerLocal.associations[material.TranscriptHash] = candidateSession
	} else if candidateSession != activeSession && attached {
		candidateSession.path = path
		candidateSession.clearCandidatePaths()
		candidateSession.pendingPing = pendingPing{}
		candidateSession.authenticated = false
		candidateSession.lastAuthenticatedPacketAt = time.Now()
	}
	peerRemote.candidateSess = candidateSession
	shouldPing := attached || candidateSession.pendingPing.challenge == 0
	if created || attached {
		peerLocal.sendHello(packet.From)
	}
	if shouldPing {
		peerLocal.sendPing(peerRemoteID, true)
	}
}

func (peerLocal *Client) evictInactiveRemotePeer() bool {
	var selectedID string
	var selectedAt time.Time
	for peerID, peerRemote := range peerLocal.remotePeers {
		if peerRemote.candidateSess != nil || (peerRemote.peerSess != nil && peerRemote.peerSess.authenticated) {
			continue
		}
		lastSeen := time.Time{}
		if peerRemote.peerSess != nil {
			lastSeen = peerRemote.peerSess.lastAuthenticatedPacketAt
		}
		if selectedID == "" || lastSeen.Before(selectedAt) {
			selectedID = peerID
			selectedAt = lastSeen
		}
	}
	if selectedID == "" {
		return false
	}
	peerRemote := peerLocal.remotePeers[selectedID]
	if peerRemote.peerSess != nil {
		delete(peerLocal.discoveredAddresses, peerRemote.peerSess.path.Address())
	}
	delete(peerLocal.remotePeers, selectedID)
	return true
}

func (peerLocal *Client) sendPings() {
	type target struct {
		peerID           string
		candidateSession bool
	}
	targets := make([]target, 0, len(peerLocal.remotePeers)*2)
	for peerID, peerRemote := range peerLocal.remotePeers {
		if peerRemote.peerSess != nil {
			targets = append(targets, target{peerID: peerID})
		}
		if peerRemote.candidateSess != nil {
			targets = append(targets, target{peerID: peerID, candidateSession: true})
		}
	}
	for _, target := range targets {
		peerLocal.sendPing(target.peerID, target.candidateSession)
	}
}

func (peerLocal *Client) sendPing(peerID string, candidateSession bool) {
	var peerSess *PeeringSession
	peerRemote := peerLocal.remotePeers[peerID]
	if peerRemote == nil {
		return
	}
	if candidateSession {
		peerSess = peerRemote.candidateSess
	} else {
		peerSess = peerRemote.peerSess
	}
	if peerSess == nil {
		return
	}
	peerLocal.sendPingOnPath(peerSess, peerSess.path, &peerSess.pendingPing)
	if !candidateSession {
		for _, probe := range peerSess.candidatePaths {
			peerLocal.sendPingOnPath(peerSess, probe.path, &probe.pendingPing)
		}
	}
}

func (peerLocal *Client) sendPingOnPath(peerSess *PeeringSession, path link.Path, pending *pendingPing) {
	now := time.Now()
	if pending.challenge != 0 && now.Sub(pending.sentAt) < pingInterval {
		return
	}
	challenge, err := randomUint64()
	if err != nil {
		return
	}
	sequence, err := peerSess.control.NextSendSequence()
	if err != nil {
		peerSess.authenticated = false
		return
	}
	*pending = pendingPing{challenge: challenge, path: path, sentAt: now}
	packet, err := protocol.MarshalControl(protocol.PacketPing, peerLocal.roomTag, peerSess.sessionID, sequence, challenge, peerSess.ciphers.ControlSend)
	roomNetwork := peerLocal.roomNetwork
	if err == nil && roomNetwork != nil {
		_ = roomNetwork.SendControl(packet, path.Address())
	}
}

func (peerLocal *Client) handleSessionPacket(packet endpoint.Datagram) {
	packetPath, err := link.NewPath(packet.From)
	if err != nil {
		return
	}
	header, err := protocol.ParseEstablishedHeader(packet.Data)
	if err != nil || (header.Type != protocol.PacketPing && header.Type != protocol.PacketPong) || header.RoomTag != peerLocal.roomTag {
		return
	}
	peerSess, candidateSession := peerLocal.sessionForControlHeader(header, packetPath)
	if peerSess == nil || !peerSess.control.MayReceive(header.Sequence) {
		return
	}
	decoded, err := protocol.ParseControl(packet.Data, peerLocal.roomTag, peerSess.sessionID, peerSess.ciphers.ControlRecv)
	if err != nil || !peerSess.control.CommitReceived(decoded.Sequence) {
		return
	}
	candidatePath := peerSess.candidatePath(packet.From)
	onCandidatePath := !candidateSession && candidatePath != nil
	pending := &peerSess.pendingPing
	if onCandidatePath {
		pending = &candidatePath.pendingPing
	}
	if decoded.Type == protocol.PacketPong {
		if pending.challenge == 0 || decoded.Challenge != pending.challenge || pending.path.Address() != packet.From {
			return
		}
	}
	wasAuthenticated := peerSess.authenticated
	promoted := false
	pathChanged := false
	now := time.Now()
	if decoded.Type == protocol.PacketPong {
		peerSess.lastAuthenticatedPacketAt = now
		peerSess.authenticated = true
		peerSess.everAuthenticated = true
		peerSess.rttMillis = max(1, now.Sub(pending.sentAt).Milliseconds())
		*pending = pendingPing{}
		peerRemote := peerLocal.remotePeerForSession(peerSess)
		if peerRemote == nil {
			return
		}
		if candidateSession {
			if peerRemote.peerSess != nil && peerRemote.peerSess != peerSess {
				delete(peerLocal.discoveredAddresses, peerRemote.peerSess.path.Address())
				peerRemote.peerSess.authenticated = false
			}
			peerRemote.peerSess = peerSess
			peerRemote.candidateSess = nil
			peerLocal.rememberDiscoveredAddress(peerSess.path.Address(), now)
			promoted = true
		} else if onCandidatePath {
			oldAddress := peerSess.path.Address()
			peerSess.path = packetPath
			peerSess.clearCandidatePaths()
			peerSess.pendingPing = pendingPing{}
			delete(peerLocal.discoveredAddresses, oldAddress)
			peerLocal.rememberDiscoveredAddress(packet.From, now)
			pathChanged = true
		}
	} else if decoded.Type == protocol.PacketPing && peerSess.everAuthenticated && !onCandidatePath {
		peerSess.lastAuthenticatedPacketAt = now
	}
	if decoded.Type == protocol.PacketPing {
		sequence, sequenceErr := peerSess.control.NextSendSequence()
		response, marshalErr := protocol.MarshalControl(protocol.PacketPong, peerLocal.roomTag, peerSess.sessionID, sequence, decoded.Challenge, peerSess.ciphers.ControlSend)
		roomNetwork := peerLocal.roomNetwork
		if sequenceErr == nil && marshalErr == nil && roomNetwork != nil {
			_ = roomNetwork.SendControl(response, packet.From)
		}
		return
	}
	remotePeerChanged := wasAuthenticated != peerSess.authenticated || promoted || pathChanged
	snapshotChanged := remotePeerChanged || decoded.Type == protocol.PacketPong
	if remotePeerChanged {
		peerLocal.logger.Info("authenticated remote peers changed", "count", peerLocal.authenticatedRemotePeerCount())
	}
	if snapshotChanged {
		peerLocal.publishStateChange()
	}
}

func (peerLocal *Client) sessionForControlHeader(header protocol.EstablishedHeader, path link.Path) (*PeeringSession, bool) {
	for _, peerRemote := range peerLocal.remotePeers {
		peerSess := peerRemote.peerSess
		if peerSess != nil && peerSess.sessionID == header.SessionID && peerSess.acceptsPath(path) {
			return peerSess, false
		}
	}
	for _, peerRemote := range peerLocal.remotePeers {
		peerSess := peerRemote.candidateSess
		if peerSess != nil && peerSess.path.Address() == path.Address() && peerSess.sessionID == header.SessionID {
			return peerSess, true
		}
	}
	return nil, false
}

func (peerLocal *Client) sessionForVoiceHeader(header protocol.EstablishedHeader, from netip.AddrPort) *PeeringSession {
	for _, peerRemote := range peerLocal.remotePeers {
		peerSess := peerRemote.peerSess
		if peerSess != nil && peerSess.path.Address() == from && peerSess.sessionID == header.SessionID {
			return peerSess
		}
	}
	return nil
}

func (peerLocal *Client) remotePeerForSession(peerSess *PeeringSession) *RemotePeer {
	for _, peerRemote := range peerLocal.remotePeers {
		if peerRemote.peerSess == peerSess || peerRemote.candidateSess == peerSess {
			return peerRemote
		}
	}
	return nil
}

func (peerLocal *Client) expireRemotePeers() {
	cutoff := time.Now().Add(-remotePeerTimeout)
	changed := false
	for peerID, peerRemote := range peerLocal.remotePeers {
		peerSess := peerRemote.peerSess
		if peerSess != nil {
			for address, probe := range peerSess.candidatePaths {
				if probe.startedAt.Before(cutoff) {
					delete(peerSess.candidatePaths, address)
				}
			}
		}
		if peerSess != nil && peerSess.lastAuthenticatedPacketAt.Before(cutoff) {
			if peerSess.everAuthenticated {
				if peerSess.authenticated {
					peerSess.authenticated = false
					peerSess.pendingPing = pendingPing{}
					changed = true
				}
			} else {
				delete(peerLocal.remotePeers, peerID)
				delete(peerLocal.discoveredAddresses, peerSess.path.Address())
			}
		}
	}
	for peerID, peerRemote := range peerLocal.remotePeers {
		peerSess := peerRemote.candidateSess
		if peerSess != nil && peerSess.lastAuthenticatedPacketAt.Before(cutoff) {
			peerRemote.candidateSess = nil
			if peerRemote.peerSess == nil {
				delete(peerLocal.remotePeers, peerID)
			}
		}
	}
	if changed {
		peerLocal.logger.Info("authenticated remote peers changed", "count", peerLocal.authenticatedRemotePeerCount())
		peerLocal.publishStateChange()
	}
}

func randomUint64() (uint64, error) {
	var encoded [8]byte
	if _, err := rand.Read(encoded[:]); err != nil {
		return 0, err
	}
	value := binary.BigEndian.Uint64(encoded[:])
	if value == 0 {
		value = 1
	}
	return value, nil
}

func (peerLocal *Client) remotePeerSnapshots() []RemotePeerSnapshot {
	remotePeers := make([]RemotePeerSnapshot, 0, len(peerLocal.remotePeers))
	for _, peerRemote := range peerLocal.remotePeers {
		peerSess := peerRemote.peerSess
		if peerSess == nil || !peerSess.authenticated {
			continue
		}
		remotePeers = append(remotePeers, RemotePeerSnapshot{
			PeerID:    peerRemote.identity.PeerID(),
			Address:   peerSess.path.Address().String(),
			SessionID: hex.EncodeToString(peerSess.sessionID[:]),
			RTTMillis: peerSess.rttMillis,
		})
	}
	sort.Slice(remotePeers, func(i, j int) bool { return remotePeers[i].PeerID < remotePeers[j].PeerID })
	return remotePeers
}

func (peerLocal *Client) authenticatedRemotePeerCount() int {
	count := 0
	for _, peerRemote := range peerLocal.remotePeers {
		if peerRemote.peerSess != nil && peerRemote.peerSess.authenticated {
			count++
		}
	}
	return count
}
