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
	"bork/internal/networking/discovery"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

const (
	helloInterval             = 2 * time.Second
	maxHelloInterval          = 8 * time.Second
	pingInterval              = time.Second
	remotePeerTimeout         = 8 * time.Second
	pathFailoverTimeout       = 2500 * time.Millisecond
	cleanupInterval           = 250 * time.Millisecond
	reliableInterval          = 20 * time.Millisecond
	maxReliablePacketsPerTick = 32
	maxRealtimeEventsPerTurn  = 8
	// maxDiscoveryHints is an untrusted-input safety budget, not a room member limit.
	maxDiscoveryHints = 2048
	topologyHintTTL   = 30 * time.Second

	likeForLikeMargin = 5
)

type discoveredAddress struct {
	source        discovery.Source
	lastSeen      time.Time
	expiresAt     time.Time
	nextProbe     time.Time
	probeInterval time.Duration
}

func (c *Client) addDiscoveryHint(hint discovery.Hint) {
	c.addDiscoveryHintAt(hint, time.Now())
}

func (c *Client) addDiscoveryHintAt(hint discovery.Hint, now time.Time) {
	address, added, changed := c.rememberDiscoveryHint(hint, now)
	if added {
		c.sendHello(address)
	}
	if changed {
		c.publishStateChange()
	}
}

func (c *Client) rememberDiscoveryHint(hint discovery.Hint, now time.Time) (netip.AddrPort, bool, bool) {
	address, valid := normalizeDiscoveryAddress(hint.Address)
	if !valid || !validDiscoverySource(hint.Source) || discoveryExpired(hint.ExpiresAt, now) ||
		((hint.Source == discovery.SourceTracker || hint.Source == discovery.SourceTopology) && hint.ExpiresAt.IsZero()) || c.isSelfAddress(address) {
		return netip.AddrPort{}, false, false
	}
	if c.discoveredAddresses == nil {
		c.discoveredAddresses = make(map[netip.AddrPort]discoveredAddress)
	}
	if remembered, exists := c.discoveredAddresses[address]; exists && discoveryExpired(remembered.expiresAt, now) {
		delete(c.discoveredAddresses, address)
	} else if exists {
		previous := remembered
		remembered.lastSeen = now
		// Never downgrade an authenticated or room-lifetime record to an expiring hint.
		if hint.Source == discovery.SourceAuthenticated || (remembered.source != discovery.SourceAuthenticated && (!remembered.expiresAt.IsZero() || hint.ExpiresAt.IsZero())) {
			remembered.source = hint.Source
			remembered.expiresAt = hint.ExpiresAt
		}
		c.discoveredAddresses[address] = remembered
		return address, false, remembered != previous
	}
	if len(c.discoveredAddresses) >= maxDiscoveryHints {
		victim, found := c.discoveryEvictionCandidate(now, hint.Source)
		if !found {
			return netip.AddrPort{}, false, false
		}
		delete(c.discoveredAddresses, victim)
	}
	c.discoveredAddresses[address] = discoveredAddress{
		source:        hint.Source,
		lastSeen:      now,
		expiresAt:     hint.ExpiresAt,
		nextProbe:     now.Add(helloInterval),
		probeInterval: helloInterval,
	}
	return address, true, true
}

func normalizeDiscoveryAddress(address netip.AddrPort) (netip.AddrPort, bool) {
	if !address.IsValid() || address.Port() == 0 {
		return netip.AddrPort{}, false
	}
	peerAddress := address.Addr().Unmap()
	if peerAddress.IsUnspecified() || peerAddress.IsMulticast() || (peerAddress.IsLinkLocalUnicast() && peerAddress.Zone() == "") {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(peerAddress, address.Port()), true
}

func validDiscoverySource(source discovery.Source) bool {
	switch source {
	case discovery.SourceLocal, discovery.SourceMDNS, discovery.SourceTracker, discovery.SourceTopology, discovery.SourceAuthenticated:
		return true
	default:
		return false
	}
}

func discoveryExpired(expiresAt, now time.Time) bool {
	return !expiresAt.IsZero() && !expiresAt.After(now)
}

func (c *Client) isSelfAddress(address netip.AddrPort) bool {
	c.snapshotMu.RLock()
	endpointSnapshot := c.networkSnapshot.Endpoint
	c.snapshotMu.RUnlock()
	if endpointHasAddress(endpointSnapshot, address) {
		return true
	}
	if c.roomNetwork != nil {
		return endpointHasAddress(c.roomNetwork.Snapshot().Endpoint, address)
	}
	return false
}

func endpointHasAddress(snapshot endpoint.Snapshot, address netip.AddrPort) bool {
	if local, err := netip.ParseAddrPort(snapshot.ListenAddress); err == nil {
		if normalized, valid := normalizeDiscoveryAddress(local); valid && normalized == address {
			return true
		}
	}
	for _, candidate := range snapshot.Candidates {
		local, err := netip.ParseAddrPort(candidate.Address)
		if err != nil {
			continue
		}
		if normalized, valid := normalizeDiscoveryAddress(local); valid && normalized == address {
			return true
		}
	}
	return false
}

func (c *Client) discoveryEvictionCandidate(now time.Time, incoming discovery.Source) (netip.AddrPort, bool) {
	var selected netip.AddrPort
	selectedRank := 6
	var selectedAt time.Time
	maximumRank := 1
	if incoming == discovery.SourceLocal || incoming == discovery.SourceMDNS {
		maximumRank = 2
	} else if incoming == discovery.SourceAuthenticated {
		maximumRank = 5
	}
	for address, remembered := range c.discoveredAddresses {
		active := c.addressInUse(address)
		rank := 5
		switch {
		case discoveryExpired(remembered.expiresAt, now):
			rank = 0
		case !active && (remembered.source == discovery.SourceTracker || remembered.source == discovery.SourceTopology):
			rank = 1
		case !active && remembered.source != discovery.SourceAuthenticated:
			rank = 2
		case !active:
			rank = 3
		case remembered.source != discovery.SourceAuthenticated:
			rank = 4
		}
		if rank > maximumRank {
			continue
		}
		if !selected.IsValid() || rank < selectedRank || (rank == selectedRank && (remembered.lastSeen.Before(selectedAt) || (remembered.lastSeen.Equal(selectedAt) && address.String() < selected.String()))) {
			selected = address
			selectedRank = rank
			selectedAt = remembered.lastSeen
		}
	}
	return selected, selected.IsValid()
}

func (c *Client) expireDiscoveryHints(now time.Time) bool {
	changed := false
	for address, remembered := range c.discoveredAddresses {
		if discoveryExpired(remembered.expiresAt, now) {
			delete(c.discoveredAddresses, address)
			changed = true
		}
	}
	return changed
}

func (c *Client) rememberAuthenticatedPath(path Path, now time.Time) {
	if path.IsDirect() {
		_, _, _ = c.rememberDiscoveryHint(discovery.Hint{Address: path.Address(), Source: discovery.SourceAuthenticated}, now)
	}
}

func (c *Client) addressInUse(address netip.AddrPort) bool {
	for _, peer := range c.remotePeers {
		for _, session := range []*PeeringSession{peer.session, peer.candidateSession} {
			if session == nil {
				continue
			}
			if session.path.Address() == address {
				return true
			}
			if session.candidatePath != nil && session.candidatePath.path.Address() == address {
				return true
			}
		}
	}
	return false
}

func (c *Client) addressHasActivePath(address netip.AddrPort) bool {
	for _, peer := range c.remotePeers {
		if peer.session != nil {
			if peer.session.authenticated && peer.session.path.Address() == address {
				return true
			}
			if peer.session.candidatePath != nil && peer.session.candidatePath.path.Address() == address {
				return true
			}
		}
		if peer.candidateSession != nil {
			if peer.candidateSession.path.Address() == address {
				return true
			}
			if peer.candidateSession.candidatePath != nil && peer.candidateSession.candidatePath.path.Address() == address {
				return true
			}
		}
	}
	return false
}

func (c *Client) rememberCandidatePath(session *PeeringSession, path Path, now time.Time) bool {
	if session.path.SameRoute(path) {
		return false
	}
	if session.candidatePath != nil {
		if session.candidatePath.path.SameRoute(path) {
			session.candidatePath.path = path
			return false
		}
		if session.candidatePath.path.IsDirect() && !path.IsDirect() {
			return false
		}
	}
	session.candidatePath = &pathProbe{path: path, startedAt: now}
	return true
}

func (c *Client) sendHello(destination netip.AddrPort) {
	path, err := NewPath(destination)
	if err != nil {
		return
	}
	c.sendHelloOnPath(path)
}

func (c *Client) sendHelloOnPath(path Path) {
	if len(c.helloPacket) == 0 {
		return
	}
	_ = c.sendControlOnPath(path, c.helloPacket)
}

func (c *Client) sendHellos(now time.Time) {
	for address, remembered := range c.discoveredAddresses {
		if discoveryExpired(remembered.expiresAt, now) || c.addressHasActivePath(address) || now.Before(remembered.nextProbe) {
			continue
		}
		remembered.probeInterval = nextHelloInterval(remembered.probeInterval)
		remembered.nextProbe = now.Add(remembered.probeInterval)
		c.discoveredAddresses[address] = remembered
		c.sendHello(address)
	}
}

func nextHelloInterval(interval time.Duration) time.Duration {
	if interval < helloInterval {
		return helloInterval
	}
	if interval >= maxHelloInterval || interval > maxHelloInterval/2 {
		return maxHelloInterval
	}
	return interval * 2
}

func (c *Client) markTopologyDirty() {
	c.topologyGeneration++
	if c.topologyGeneration == 0 {
		c.topologyGeneration = 1
	}
}

func (c *Client) markPeerGraphDirty(topologyChanged bool) {
	c.fanoutDirty = true
	if topologyChanged {
		c.markTopologyDirty()
	}
}

func shouldPromotePath(current Path, currentAuthenticated bool, currentRTT int64, currentLastAuthenticated, now time.Time, candidate Path, candidateRTT int64) bool {
	if !currentAuthenticated || currentLastAuthenticated.Before(now.Add(-pathFailoverTimeout)) {
		return true
	}
	if current.IsDirect() && !candidate.IsDirect() {
		return false
	}
	if !current.IsDirect() && candidate.IsDirect() {
		return true
	}
	return candidateRTT+likeForLikeMargin <= currentRTT
}

func rawPeerIdentity(peerIdentity identity.Identity) [32]byte {
	var encoded [32]byte
	copy(encoded[:], peerIdentity.PublicKey())
	return encoded
}

func usableTopologyAddress(address, source netip.AddrPort) bool {
	address, valid := normalizeDiscoveryAddress(address)
	if !valid {
		return false
	}
	ip := address.Addr().Unmap()
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	return !ip.IsPrivate() || source.Addr().Unmap().IsPrivate()
}

func (c *Client) handlePacket(packet endpoint.Datagram, mediaPort media.PeerPort) {
	packetType, roomTag, err := protocol.ParsePrefix(packet.Data)
	if err != nil || roomTag != c.roomTag {
		return
	}
	switch packetType {
	case protocol.PacketHello:
		c.handleHello(packet)
	case protocol.PacketPing, protocol.PacketPong:
		c.handleSessionPacket(packet)
	case protocol.PacketReliable:
		path, pathErr := NewPath(packet.From)
		if pathErr == nil {
			c.handleReliablePacketOnPath(packet.Data, path)
		}
	case protocol.PacketRoomDatagram:
		c.handleRoomDatagram(packet, mediaPort)
	case protocol.PacketBridgeControl:
		c.handleBridgePacket(packet)
	}
}

func (c *Client) sendReliable(now time.Time) {
	peerIDs := make([]string, 0, len(c.remotePeers))
	for peerID, peer := range c.remotePeers {
		if peer.session != nil && peer.session.authenticated && peer.session.reliable != nil {
			peerIDs = append(peerIDs, peerID)
		}
	}
	if len(peerIDs) == 0 {
		return
	}
	sort.Strings(peerIDs)
	start := sort.Search(len(peerIDs), func(index int) bool { return peerIDs[index] > c.reliablePeerCursor })
	if start == len(peerIDs) {
		start = 0
	}

	remaining := maxReliablePacketsPerTick
	for remaining > 0 {
		progress := false
		for offset := range len(peerIDs) {
			peerID := peerIDs[(start+offset)%len(peerIDs)]
			peer := c.remotePeers[peerID]
			session := peer.session
			if session == nil || !session.authenticated || session.reliable == nil {
				continue
			}
			packet, reservation, ok := session.reliable.nextSend(now)
			if !ok {
				continue
			}
			sequence, err := session.control.nextSendSequence()
			if err != nil {
				session.authenticated = false
				c.markPeerGraphDirty(session.path.IsDirect())
				continue
			}
			encoded, err := protocol.MarshalReliable(c.roomTag, session.sessionID, sequence, packet, session.ciphers.ControlSend)
			background := packet.Channel == reliableChannelFileData && !packet.AckOnly()
			if err != nil || c.sendPacketOnPath(session.path, encoded, background) != nil {
				session.reliable.reject(reservation)
				continue
			}
			session.reliable.commit(reservation)
			c.reliablePeerCursor = peerID
			remaining--
			progress = true
			if remaining == 0 {
				return
			}
		}
		if !progress {
			return
		}
	}
}

func (c *Client) handleReliablePacketOnPath(data []byte, path Path) {
	header, err := protocol.ParseEstablishedHeader(data)
	if err != nil || header.Type != protocol.PacketReliable || header.RoomTag != c.roomTag {
		return
	}
	sender, session, candidate := c.sessionForControlHeader(header, path)
	if session == nil || candidate || !session.authenticated || !session.acceptsDataPath(path) || !session.control.mayReceive(header.Sequence) {
		return
	}
	decoded, err := protocol.ParseReliable(data, c.roomTag, session.sessionID, session.ciphers.ControlRecv)
	if err != nil || !session.control.commitReceived(header.Sequence) {
		return
	}
	now := time.Now()
	session.lastAuthenticatedPacketAt = now
	c.rememberAuthenticatedPath(path, now)
	for _, message := range session.reliable.receive(decoded, now) {
		c.handleReliableMessage(sender, message)
	}
}

func (c *Client) handleHello(packet endpoint.Datagram) {
	path, err := NewPath(packet.From)
	if err != nil {
		return
	}
	c.handleHelloOnPath(packet.Data, path)
}

func (c *Client) handleHelloOnPath(data []byte, path Path) {
	hello, err := protocol.ParseHello(data, c.roomTag, c.admissionKey)
	if err != nil {
		return
	}
	remotePeerIdentity, err := identity.FromPublicKey(hello.IdentityKey)
	if err != nil || remotePeerIdentity.PeerID() == c.localIdentity.PeerID() {
		return
	}
	remotePeerID := remotePeerIdentity.PeerID()
	material, err := protocol.DeriveSession(c.ephemeralPrivateKey, c.localHello, hello)
	if err != nil {
		return
	}

	remotePeer := c.remotePeers[remotePeerID]
	if remotePeer == nil {
		remotePeer = &RemotePeer{identity: remotePeerIdentity}
		c.remotePeers[remotePeerID] = remotePeer
	}
	now := time.Now()
	activeSession := remotePeer.session
	if activeSession != nil && activeSession.sessionID == material.SessionID {
		if c.rememberCandidatePath(activeSession, path, now) {
			c.sendHelloOnPath(path)
		}
		c.sendPing(remotePeerID, false)
		return
	}

	candidateSession := remotePeer.candidateSession
	if candidateSession == nil || candidateSession.sessionID != material.SessionID {
		candidateSession = newPeeringSession(path, material, now)
		remotePeer.candidateSession = candidateSession
		candidateSession.lastHelloSentAt = now
		c.sendHelloOnPath(path)
		c.sendPing(remotePeerID, true)
		return
	}
	if c.rememberCandidatePath(candidateSession, path, now) {
		candidateSession.lastHelloSentAt = now
		c.sendHelloOnPath(path)
	}
	c.sendPing(remotePeerID, true)
}

func (c *Client) sendPings() {
	type target struct {
		peerID           string
		candidateSession bool
	}
	targets := make([]target, 0, len(c.remotePeers)*2)
	for peerID, peer := range c.remotePeers {
		if peer.session != nil {
			targets = append(targets, target{peerID: peerID})
		}
		if peer.candidateSession != nil {
			targets = append(targets, target{peerID: peerID, candidateSession: true})
		}
	}
	for _, target := range targets {
		c.sendPing(target.peerID, target.candidateSession)
	}
}

func (c *Client) sendPing(peerID string, candidateSession bool) {
	var peerSess *PeeringSession
	peer := c.remotePeers[peerID]
	if peer == nil {
		return
	}
	if candidateSession {
		peerSess = peer.candidateSession
	} else {
		peerSess = peer.session
	}
	if peerSess == nil {
		return
	}
	if candidateSession {
		now := time.Now()
		if now.Sub(peerSess.lastHelloSentAt) >= helloInterval {
			peerSess.lastHelloSentAt = now
			c.sendHelloOnPath(peerSess.path)
		}
	}
	c.sendPingOnPath(peerSess, peerSess.path, &peerSess.pendingPing)
	if peerSess.candidatePath != nil {
		c.sendPingOnPath(peerSess, peerSess.candidatePath.path, &peerSess.candidatePath.pendingPing)
	}
}

func (c *Client) sendPingOnPath(peerSess *PeeringSession, path Path, pending *pendingPing) {
	now := time.Now()
	if pending.challenge != 0 && now.Sub(pending.sentAt) < pingInterval {
		return
	}
	challenge, err := randomUint64()
	if err != nil {
		return
	}
	sequence, err := peerSess.control.nextSendSequence()
	if err != nil {
		if peerSess.authenticated {
			peerSess.authenticated = false
			c.markPeerGraphDirty(peerSess.path.IsDirect())
		}
		return
	}
	*pending = pendingPing{challenge: challenge, path: path, sentAt: now}
	packet, err := protocol.MarshalControl(protocol.PacketPing, c.roomTag, peerSess.sessionID, sequence, challenge, peerSess.ciphers.ControlSend)
	if err == nil {
		_ = c.sendControlOnPath(path, packet)
	}
}

func (c *Client) handleSessionPacket(packet endpoint.Datagram) {
	packetPath, err := NewPath(packet.From)
	if err != nil {
		return
	}
	c.handleSessionPacketOnPath(packet.Data, packetPath)
}

func (c *Client) handleSessionPacketOnPath(data []byte, packetPath Path) {
	header, err := protocol.ParseEstablishedHeader(data)
	if err != nil || (header.Type != protocol.PacketPing && header.Type != protocol.PacketPong) || header.RoomTag != c.roomTag {
		return
	}
	remotePeer, peerSess, candidateSession := c.sessionForControlHeader(header, packetPath)
	if peerSess == nil || !peerSess.control.mayReceive(header.Sequence) {
		return
	}
	decoded, err := protocol.ParseControl(data, c.roomTag, peerSess.sessionID, peerSess.ciphers.ControlRecv)
	if err != nil || !peerSess.control.commitReceived(header.Sequence) {
		return
	}
	candidatePath := peerSess.candidateProbe(packetPath)
	onCandidatePath := candidatePath != nil
	pending := &peerSess.pendingPing
	if onCandidatePath {
		pending = &candidatePath.pendingPing
	}
	if decoded.Type == protocol.PacketPong {
		if pending.challenge == 0 || decoded.Challenge != pending.challenge || !pending.path.SameRoute(packetPath) {
			return
		}
	}
	wasAuthenticated := peerSess.authenticated
	beforeDirectPath, hadDirectPath := authenticatedDirectPath(remotePeer)
	promoted := false
	pathChanged := false
	now := time.Now()
	if decoded.Type == protocol.PacketPong {
		rttMillis := max(1, now.Sub(pending.sentAt).Milliseconds())
		*pending = pendingPing{}
		if onCandidatePath {
			c.rememberAuthenticatedPath(packetPath, now)
			if shouldPromotePath(peerSess.path, peerSess.authenticated, peerSess.rttMillis, peerSess.lastAuthenticatedPacketAt, now, packetPath, rttMillis) {
				peerSess.clearCandidatePath()
				peerSess.path = packetPath
				peerSess.lastAuthenticatedPacketAt = now
				peerSess.rttMillis = rttMillis
				peerSess.authenticated = true
				peerSess.everAuthenticated = true
				peerSess.pendingPing = pendingPing{}
				pathChanged = true
			} else {
				peerSess.clearCandidatePath()
			}
		} else {
			if peerSess.path.SameRoute(packetPath) {
				peerSess.path = packetPath
			}
			peerSess.lastAuthenticatedPacketAt = now
			peerSess.authenticated = true
			peerSess.everAuthenticated = true
			c.rememberAuthenticatedPath(packetPath, now)
			peerSess.rttMillis = rttMillis
		}
		if candidateSession {
			if remotePeer.session != nil && remotePeer.session != peerSess {
				remotePeer.session.authenticated = false
			}
			remotePeer.session = peerSess
			remotePeer.candidateSession = nil
			promoted = true
		}
	} else if decoded.Type == protocol.PacketPing && peerSess.everAuthenticated && !onCandidatePath {
		if peerSess.path.SameRoute(packetPath) {
			peerSess.path = packetPath
		}
		peerSess.lastAuthenticatedPacketAt = now
		c.rememberAuthenticatedPath(packetPath, now)
	}
	if decoded.Type == protocol.PacketPing {
		sequence, sequenceErr := peerSess.control.nextSendSequence()
		response, marshalErr := protocol.MarshalControl(protocol.PacketPong, c.roomTag, peerSess.sessionID, sequence, decoded.Challenge, peerSess.ciphers.ControlSend)
		if sequenceErr == nil && marshalErr == nil {
			_ = c.sendControlOnPath(packetPath, response)
		}
		return
	}
	remotePeerChanged := wasAuthenticated != peerSess.authenticated || promoted || pathChanged
	snapshotChanged := remotePeerChanged || decoded.Type == protocol.PacketPong
	if peerSess.authenticated && (!wasAuthenticated || promoted) {
		c.queueMemberStates()
		c.queueScreenStates()
	}
	if remotePeerChanged {
		c.rememberTopologyPeer(remotePeer.identity, now)
		afterDirectPath, hasDirectPath := authenticatedDirectPath(remotePeer)
		c.markPeerGraphDirty(hadDirectPath != hasDirectPath || (hasDirectPath && beforeDirectPath != afterDirectPath))
		c.queueTopologySnapshots(now, true)
		c.logger.Info("authenticated remote peers changed", "count", c.authenticatedRemotePeerCount())
	}
	if snapshotChanged {
		c.publishStateChange()
	}
}

func (c *Client) sessionForControlHeader(header protocol.EstablishedHeader, path Path) (*RemotePeer, *PeeringSession, bool) {
	for _, peer := range c.remotePeers {
		peerSess := peer.session
		if peerSess != nil && peerSess.sessionID == header.SessionID && peerSess.acceptsPath(path) {
			return peer, peerSess, false
		}
	}
	for _, peer := range c.remotePeers {
		peerSess := peer.candidateSession
		if peerSess != nil && peerSess.acceptsPath(path) && peerSess.sessionID == header.SessionID {
			return peer, peerSess, true
		}
	}
	return nil, nil, false
}

func (c *Client) expireRemotePeers() {
	now := time.Now()
	cutoff := now.Add(-remotePeerTimeout)
	failoverCutoff := now.Add(-pathFailoverTimeout)
	changed := false
	topologyChanged := false
	for peerID, peer := range c.remotePeers {
		peerSess := peer.session
		if peerSess != nil {
			if probe := peerSess.candidatePath; probe != nil {
				if probe.startedAt.Before(cutoff) {
					peerSess.clearCandidatePath()
				}
			}
			if peerSess.authenticated && peerSess.lastAuthenticatedPacketAt.Before(failoverCutoff) {
				topologyChanged = topologyChanged || peerSess.path.IsDirect()
				peerSess.authenticated = false
				peerSess.pendingPing = pendingPing{}
				peerSess.clearCandidatePath()
				changed = true
			}
		}
		if peerSess != nil && peerSess.lastAuthenticatedPacketAt.Before(cutoff) {
			peer.session = nil
			if peer.candidateSession == nil {
				delete(c.remotePeers, peerID)
			}
		}
	}
	for peerID, peer := range c.remotePeers {
		peerSess := peer.candidateSession
		if peerSess != nil && peerSess.lastAuthenticatedPacketAt.Before(cutoff) {
			peer.candidateSession = nil
			if peer.session == nil {
				delete(c.remotePeers, peerID)
			}
		}
	}
	if changed {
		c.markPeerGraphDirty(topologyChanged)
		c.logger.Info("authenticated remote peers changed", "count", c.authenticatedRemotePeerCount())
		c.publishStateChange()
	}
}

func authenticatedDirectPath(peer *RemotePeer) (Path, bool) {
	if peer == nil || peer.session == nil || !peer.session.authenticated || !peer.session.path.IsDirect() {
		return Path{}, false
	}
	return peer.session.path, true
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

func (c *Client) remotePeerSnapshots() []RemotePeerSnapshot {
	remotePeers := make([]RemotePeerSnapshot, 0, len(c.remotePeers))
	for _, peer := range c.remotePeers {
		peerSess := peer.session
		if peerSess == nil || !peerSess.authenticated {
			continue
		}
		transport := "direct"
		if !peerSess.path.IsDirect() {
			transport = "bridge"
		}
		remotePeers = append(remotePeers, RemotePeerSnapshot{
			PeerID:           peer.identity.PeerID(),
			Address:          peerSess.path.Address().String(),
			SessionID:        hex.EncodeToString(peerSess.sessionID[:]),
			RTTMillis:        peerSess.rttMillis,
			Transport:        transport,
			Nickname:         peerSess.remoteMemberState.nickname,
			Muted:            peerSess.remoteMemberState.muted,
			PlaybackMuted:    peerSess.remoteMemberState.playbackMuted,
			ScreenSharing:    peerSess.remoteScreenState.active,
			ScreenGeneration: peerSess.remoteScreenState.generation,
			ScreenStreamID:   hex.EncodeToString(peerSess.remoteScreenState.streamID[:]),
		})
	}
	sort.Slice(remotePeers, func(i, j int) bool { return remotePeers[i].PeerID < remotePeers[j].PeerID })
	return remotePeers
}

func (c *Client) authenticatedRemotePeerCount() int {
	count := 0
	for _, peer := range c.remotePeers {
		if peer.session != nil && peer.session.authenticated {
			count++
		}
	}
	return count
}
