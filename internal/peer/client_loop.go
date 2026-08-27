package peer

import (
	"bytes"
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
	discoveryProbeInterval    = 2 * time.Second
	maxDiscoveryProbeInterval = 8 * time.Second
	sessionHelloInterval      = 2 * time.Second
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
		c.sendHelloProbe(address)
	}
	if changed {
		c.publishStateChange()
	}
}

func (c *Client) rememberDiscoveryHint(hint discovery.Hint, now time.Time) (netip.AddrPort, bool, bool) {
	address, valid := normalizeDiscoveryAddress(hint.Address)
	if !valid || !validDiscoverySource(hint.Source) || discoveryExpired(hint.ExpiresAt, now) ||
		((hint.Source == discovery.SourceTracker || hint.Source == discovery.SourceTopology || hint.Source == discovery.SourceHistoricalRemote) && hint.ExpiresAt.IsZero()) || c.isSelfAddress(address) {
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
		// Room-lifetime local discovery outranks expiring sources. Historical
		// remote addresses in turn outrank unverified tracker and topology hints.
		if hint.ExpiresAt.IsZero() || (!remembered.expiresAt.IsZero() && (hint.Source == discovery.SourceHistoricalRemote || remembered.source != discovery.SourceHistoricalRemote)) {
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
		nextProbe:     now.Add(discoveryProbeInterval),
		probeInterval: discoveryProbeInterval,
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
	case discovery.SourceLocal, discovery.SourceMDNS, discovery.SourceTracker, discovery.SourceTopology, discovery.SourceHistoricalRemote:
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
	} else if incoming == discovery.SourceHistoricalRemote {
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
		case !active && remembered.source != discovery.SourceHistoricalRemote:
			rank = 2
		case !active:
			rank = 3
		case remembered.source != discovery.SourceHistoricalRemote:
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
		_, _, _ = c.rememberDiscoveryHint(discovery.Hint{
			Address: path.Address(), Source: discovery.SourceHistoricalRemote, ExpiresAt: now.Add(knownPeerTTL),
		}, now)
	}
}

func (c *Client) addressInUse(address netip.AddrPort) bool {
	for _, peer := range c.remotePeers {
		for _, session := range []*Session{peer.activeSession, peer.pendingSession} {
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

func (c *Client) addressHasSessionPath(address netip.AddrPort) bool {
	for _, peer := range c.remotePeers {
		if peer.activeSession != nil {
			if peer.activeSession.authenticated && peer.activeSession.path.Address() == address {
				return true
			}
			if peer.activeSession.candidatePath != nil && peer.activeSession.candidatePath.path.Address() == address {
				return true
			}
		}
		if peer.pendingSession != nil {
			if peer.pendingSession.path.Address() == address {
				return true
			}
			if peer.pendingSession.candidatePath != nil && peer.pendingSession.candidatePath.path.Address() == address {
				return true
			}
		}
	}
	return false
}

func (c *Client) rememberCandidatePath(session *Session, path Path, now time.Time) bool {
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

func (c *Client) sendHelloProbe(destination netip.AddrPort) {
	path, err := NewPath(destination)
	if err != nil {
		return
	}
	c.sendHelloProbeOnPath(path)
}

func (c *Client) sendHelloProbeOnPath(path Path) {
	if len(c.helloProbePacket) == 0 {
		return
	}
	_ = c.sendPeerPacketOnPath(path, c.helloProbePacket)
}

func (c *Client) sendSessionHelloOnPath(session *Session, path Path) {
	if session == nil || len(session.localHelloPacket) == 0 {
		return
	}
	_ = c.sendPeerPacketOnPath(path, session.localHelloPacket)
}

func (c *Client) sendDiscoveryProbes(now time.Time) {
	for address, remembered := range c.discoveredAddresses {
		if discoveryExpired(remembered.expiresAt, now) || c.addressHasSessionPath(address) || now.Before(remembered.nextProbe) {
			continue
		}
		remembered.probeInterval = nextDiscoveryProbeInterval(remembered.probeInterval)
		remembered.nextProbe = now.Add(remembered.probeInterval)
		c.discoveredAddresses[address] = remembered
		c.sendHelloProbe(address)
	}
}

func nextDiscoveryProbeInterval(interval time.Duration) time.Duration {
	if interval < discoveryProbeInterval {
		return discoveryProbeInterval
	}
	if interval >= maxDiscoveryProbeInterval || interval > maxDiscoveryProbeInterval/2 {
		return maxDiscoveryProbeInterval
	}
	return interval * 2
}

func (c *Client) markTopologyDirty() {
	c.topologyRevision++
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
	packetType, err := protocol.ParsePrefix(packet.Data)
	if err != nil {
		return
	}
	switch packetType {
	case protocol.PacketHelloProbe, protocol.PacketSessionHello:
		c.handleHello(packet)
	case protocol.PacketPing, protocol.PacketPong, protocol.PacketLeave:
		c.handleSessionControlPacket(packet, packetType)
	case protocol.PacketReliable:
		path, pathErr := NewPath(packet.From)
		if pathErr == nil {
			c.handleReliablePacketOnPath(packet.Data, path)
		}
	case protocol.PacketVoice, protocol.PacketScreenVideo, protocol.PacketScreenAudio:
		c.handleRoomDatagram(packet, mediaPort)
	case protocol.PacketBridge, protocol.PacketBridgeLowPriority:
		c.handleBridgePacket(packet)
	}
}

func (c *Client) handleSessionControlPacket(packet endpoint.Datagram, packetType protocol.PacketType) {
	path, err := NewPath(packet.From)
	if err != nil {
		return
	}
	if packetType == protocol.PacketLeave {
		c.handleLeavePacketOnPath(packet.Data, path)
		return
	}
	c.handleSessionPacketOnPath(packet.Data, path)
}

func (c *Client) sendReliable(now time.Time) {
	peerIDs := make([]identity.PeerID, 0, len(c.remotePeers))
	for peerID, peer := range c.remotePeers {
		if peer.activeSession != nil && peer.activeSession.authenticated && peer.activeSession.reliable != nil {
			peerIDs = append(peerIDs, peerID)
		}
	}
	if len(peerIDs) == 0 {
		return
	}
	sort.Slice(peerIDs, func(left, right int) bool {
		return bytes.Compare(peerIDs[left][:], peerIDs[right][:]) < 0
	})
	start := sort.Search(len(peerIDs), func(index int) bool {
		return bytes.Compare(peerIDs[index][:], c.reliablePeerCursor[:]) > 0
	})
	if start == len(peerIDs) {
		start = 0
	}

	remaining := maxReliablePacketsPerTick
	for remaining > 0 {
		progress := false
		for offset := range len(peerIDs) {
			peerID := peerIDs[(start+offset)%len(peerIDs)]
			peer := c.remotePeers[peerID]
			activeSession := peer.activeSession
			if activeSession == nil || !activeSession.authenticated || activeSession.reliable == nil {
				continue
			}
			packet, reservation, ok := activeSession.reliable.nextSend(now)
			if !ok {
				continue
			}
			sequence, err := activeSession.packetFlow.nextSendSequence()
			if err != nil {
				activeSession.authenticated = false
				c.markPeerGraphDirty(activeSession.path.IsDirect())
				continue
			}
			encoded, err := protocol.MarshalReliable(activeSession.id(), sequence, packet, activeSession.ciphers.Send)
			lowPriority := packet.Channel == reliableChannelFileData && !packet.AckOnly()
			if err != nil || c.sendPacketOnPath(activeSession.path, encoded, lowPriority) != nil {
				activeSession.reliable.reject(reservation)
				continue
			}
			activeSession.reliable.commit(reservation)
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
	header, err := protocol.ParseSessionHeader(data)
	if err != nil || header.Type != protocol.PacketReliable {
		return
	}
	sender, session, isPendingSession := c.sessionForHeader(header, path)
	if session == nil || isPendingSession || !session.authenticated || !session.acceptsDataPath(path) || !session.packetFlow.mayReceive(header.PacketSequence) {
		return
	}
	decoded, err := protocol.ParseReliable(data, session.id(), session.ciphers.Receive)
	if err != nil || !session.packetFlow.commitReceived(header.PacketSequence) {
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
	packetType, err := protocol.ParsePrefix(data)
	if err != nil {
		return
	}
	now := time.Now()
	if packetType == protocol.PacketHelloProbe {
		peerID, parseErr := protocol.ParseHelloProbe(data, c.admissionKey)
		if parseErr != nil || peerID == c.localPeerID {
			return
		}
		remotePeer := c.remotePeers[peerID]
		if remotePeer == nil {
			remotePeer = &RemotePeer{peerID: peerID}
		}
		c.handleHelloProbe(remotePeer, path, now)
		return
	}
	if packetType != protocol.PacketSessionHello {
		return
	}
	hello, err := protocol.ParseSessionHello(data, c.admissionKey)
	if err != nil || hello.PeerID == c.localPeerID {
		return
	}
	remotePeer := c.remotePeers[hello.PeerID]
	if remotePeer == nil {
		remotePeer = &RemotePeer{peerID: hello.PeerID}
	}
	c.handleSessionHello(remotePeer, hello, path, now)
}

func (c *Client) handleHelloProbe(remotePeer *RemotePeer, path Path, now time.Time) {
	session, usePendingSession := remotePeer.pendingSession, true
	if session == nil {
		session, usePendingSession = remotePeer.activeSession, false
	}
	if session != nil {
		c.rememberCandidatePath(session, path, now)
		c.sendSessionHelloOnPath(session, path)
		c.sendPing(remotePeer.peerID, usePendingSession)
		return
	}
	// One deterministic initiator prevents simultaneous probes from creating
	// two unrelated transcripts for the same pair of peers.
	if bytes.Compare(c.localPeerID[:], remotePeer.peerID[:]) < 0 {
		c.startInitiatingSession(remotePeer, path, now)
		return
	}
	c.sendHelloProbeOnPath(path)
}

func (c *Client) handleSessionHello(remotePeer *RemotePeer, hello protocol.SessionHello, path Path, now time.Time) {
	if c.resumeSessionFromHello(remotePeer, remotePeer.activeSession, hello, path, now, false) {
		return
	}
	if c.resumeSessionFromHello(remotePeer, remotePeer.pendingSession, hello, path, now, true) {
		return
	}
	if bytes.Compare(c.localPeerID[:], remotePeer.peerID[:]) < 0 {
		c.handleUnexpectedResponderHello(remotePeer, hello, path, now)
		return
	}
	session, err := c.newSessionWithLocalHello(path, hello.SessionID, now)
	if err != nil {
		return
	}
	if err := session.completeSessionHello(hello); err != nil {
		return
	}
	remotePeer.pendingSession = session
	c.remotePeers[remotePeer.peerID] = remotePeer
	session.lastSessionHelloSentAt = now
	c.sendSessionHelloOnPath(session, path)
	c.sendPing(remotePeer.peerID, true)
}

func (c *Client) resumeSessionFromHello(remotePeer *RemotePeer, session *Session, hello protocol.SessionHello, path Path, now time.Time, pending bool) bool {
	if session == nil {
		return false
	}
	if !session.sessionReady() && session.id() == hello.SessionID {
		if err := session.completeSessionHello(hello); err != nil {
			return false
		}
	} else if !session.matchesRemoteHello(hello) {
		return false
	}
	c.rememberCandidatePath(session, path, now)
	session.lastSessionHelloSentAt = now
	c.sendSessionHelloOnPath(session, path)
	c.sendPing(remotePeer.peerID, pending)
	return true
}

func (c *Client) handleUnexpectedResponderHello(remotePeer *RemotePeer, hello protocol.SessionHello, path Path, now time.Time) {
	current := remotePeer.pendingSession
	if current == nil {
		current = remotePeer.activeSession
	}
	if current != nil && current.id() != hello.SessionID {
		// A late Hello must not replace a newer transcript. Re-advertising the
		// current Hello is enough for the deterministic initiator to converge.
		c.sendSessionHelloOnPath(current, path)
		return
	}
	c.startInitiatingSession(remotePeer, path, now)
}

func (c *Client) startInitiatingSession(remotePeer *RemotePeer, path Path, now time.Time) {
	session, err := c.newInitiatingSession(path, now)
	if err != nil {
		return
	}
	remotePeer.pendingSession = session
	c.remotePeers[remotePeer.peerID] = remotePeer
	session.lastSessionHelloSentAt = now
	c.sendSessionHelloOnPath(session, path)
}

func (c *Client) sendPings() {
	type target struct {
		peerID            identity.PeerID
		usePendingSession bool
	}
	targets := make([]target, 0, len(c.remotePeers)*2)
	for peerID, peer := range c.remotePeers {
		if peer.activeSession != nil {
			targets = append(targets, target{peerID: peerID})
		}
		if peer.pendingSession != nil {
			targets = append(targets, target{peerID: peerID, usePendingSession: true})
		}
	}
	for _, target := range targets {
		c.sendPing(target.peerID, target.usePendingSession)
	}
}

func (c *Client) sendPing(peerID identity.PeerID, usePendingSession bool) {
	var session *Session
	peer := c.remotePeers[peerID]
	if peer == nil {
		return
	}
	if usePendingSession {
		session = peer.pendingSession
	} else {
		session = peer.activeSession
	}
	if session == nil {
		return
	}
	if usePendingSession {
		now := time.Now()
		if now.Sub(session.lastSessionHelloSentAt) >= sessionHelloInterval {
			session.lastSessionHelloSentAt = now
			c.sendSessionHelloOnPath(session, session.path)
		}
	}
	if !session.sessionReady() {
		return
	}
	c.sendPingOnPath(session, session.path, &session.pendingPing)
	if session.candidatePath != nil {
		c.sendPingOnPath(session, session.candidatePath.path, &session.candidatePath.pendingPing)
	}
}

func (c *Client) sendPingOnPath(session *Session, path Path, pending *pendingPing) {
	now := time.Now()
	if pending.packetSequence != 0 && now.Sub(pending.sentAt) < pingInterval {
		return
	}
	sequence, err := session.packetFlow.nextSendSequence()
	if err != nil {
		if session.authenticated {
			session.authenticated = false
			c.markPeerGraphDirty(session.path.IsDirect())
		}
		return
	}
	*pending = pendingPing{packetSequence: sequence, path: path, sentAt: now}
	packet, err := protocol.MarshalControl(protocol.PacketPing, session.id(), sequence, 0, session.ciphers.Send)
	if err == nil {
		_ = c.sendPeerPacketOnPath(path, packet)
	}
}

func (c *Client) handleSessionPacketOnPath(data []byte, packetPath Path) {
	header, err := protocol.ParseSessionHeader(data)
	if err != nil || (header.Type != protocol.PacketPing && header.Type != protocol.PacketPong) {
		return
	}
	remotePeer, session, isPendingSession := c.sessionForHeader(header, packetPath)
	if session == nil || !session.packetFlow.mayReceive(header.PacketSequence) {
		return
	}
	pingSequence, err := protocol.ParseControl(data, session.id(), session.ciphers.Receive)
	if err != nil || !session.packetFlow.commitReceived(header.PacketSequence) {
		return
	}
	candidatePath := session.candidateProbe(packetPath)
	onCandidatePath := candidatePath != nil
	pending := &session.pendingPing
	if onCandidatePath {
		pending = &candidatePath.pendingPing
	}
	if header.Type == protocol.PacketPong {
		if pending.packetSequence == 0 || pingSequence != pending.packetSequence || !pending.path.SameRoute(packetPath) {
			return
		}
	}
	wasAuthenticated := session.authenticated
	beforeDirectPath, hadDirectPath := authenticatedDirectPath(remotePeer)
	promoted := false
	pathChanged := false
	now := time.Now()
	if header.Type == protocol.PacketPong {
		rttMillis := max(1, now.Sub(pending.sentAt).Milliseconds())
		*pending = pendingPing{}
		if onCandidatePath {
			c.rememberAuthenticatedPath(packetPath, now)
			if shouldPromotePath(session.path, session.authenticated, session.rttMillis, session.lastAuthenticatedPacketAt, now, packetPath, rttMillis) {
				session.clearCandidatePath()
				session.path = packetPath
				session.lastAuthenticatedPacketAt = now
				session.rttMillis = rttMillis
				session.authenticated = true
				session.everAuthenticated = true
				session.pendingPing = pendingPing{}
				pathChanged = true
			} else {
				session.clearCandidatePath()
			}
		} else {
			if session.path.SameRoute(packetPath) {
				session.path = packetPath
			}
			session.lastAuthenticatedPacketAt = now
			session.authenticated = true
			session.everAuthenticated = true
			c.rememberAuthenticatedPath(packetPath, now)
			session.rttMillis = rttMillis
		}
		if isPendingSession {
			if remotePeer.activeSession != nil && remotePeer.activeSession != session {
				remotePeer.activeSession.authenticated = false
			}
			remotePeer.activeSession = session
			remotePeer.pendingSession = nil
			promoted = true
		}
	} else if header.Type == protocol.PacketPing && session.everAuthenticated && !onCandidatePath {
		if session.path.SameRoute(packetPath) {
			session.path = packetPath
		}
		session.lastAuthenticatedPacketAt = now
		c.rememberAuthenticatedPath(packetPath, now)
	}
	if header.Type == protocol.PacketPing {
		sequence, sequenceErr := session.packetFlow.nextSendSequence()
		response, marshalErr := protocol.MarshalControl(protocol.PacketPong, session.id(), sequence, header.PacketSequence, session.ciphers.Send)
		if sequenceErr == nil && marshalErr == nil {
			_ = c.sendPeerPacketOnPath(packetPath, response)
		}
		return
	}
	remotePeerChanged := wasAuthenticated != session.authenticated || promoted || pathChanged
	snapshotChanged := remotePeerChanged || header.Type == protocol.PacketPong
	if session.authenticated && (!wasAuthenticated || promoted) {
		c.queueMemberStates()
		c.queueScreenStates()
	}
	if remotePeerChanged {
		c.rememberTopologyPeer(remotePeer.peerID, now)
		afterDirectPath, hasDirectPath := authenticatedDirectPath(remotePeer)
		c.markPeerGraphDirty(hadDirectPath != hasDirectPath || (hasDirectPath && beforeDirectPath != afterDirectPath))
		c.queueTopologySnapshots(now, true)
		c.logger.Info("authenticated remote peers changed", "count", c.authenticatedRemotePeerCount())
	}
	if snapshotChanged {
		c.publishStateChange()
	}
}

func (c *Client) sessionForHeader(header protocol.SessionHeader, path Path) (*RemotePeer, *Session, bool) {
	for _, peer := range c.remotePeers {
		session := peer.activeSession
		if session != nil && session.sessionReady() && session.id() == header.SessionID && session.acceptsPath(path) {
			return peer, session, false
		}
	}
	for _, peer := range c.remotePeers {
		session := peer.pendingSession
		if session != nil && session.sessionReady() && session.acceptsPath(path) && session.id() == header.SessionID {
			return peer, session, true
		}
	}
	return nil, nil, false
}

func (c *Client) expireRemotePeers() {
	now := time.Now()
	cutoff := now.Add(-remotePeerTimeout)
	failoverCutoff := now.Add(-pathFailoverTimeout)
	changed := false
	snapshotChanged := false
	topologyChanged := false
	for peerID, peer := range c.remotePeers {
		activeSession := peer.activeSession
		if activeSession != nil {
			if probe := activeSession.candidatePath; probe != nil {
				if probe.startedAt.Before(cutoff) {
					activeSession.clearCandidatePath()
				}
			}
			if activeSession.authenticated && activeSession.lastAuthenticatedPacketAt.Before(failoverCutoff) {
				topologyChanged = topologyChanged || activeSession.path.IsDirect()
				activeSession.authenticated = false
				activeSession.pendingPing = pendingPing{}
				activeSession.clearCandidatePath()
				changed = true
				snapshotChanged = true
			}
		}
		if activeSession != nil && activeSession.lastAuthenticatedPacketAt.Before(cutoff) {
			snapshotChanged = snapshotChanged || activeSession.everAuthenticated
			peer.activeSession = nil
			if peer.pendingSession == nil {
				delete(c.remotePeers, peerID)
			}
		}
	}
	for peerID, peer := range c.remotePeers {
		pendingSession := peer.pendingSession
		if pendingSession != nil && pendingSession.lastAuthenticatedPacketAt.Before(cutoff) {
			peer.pendingSession = nil
			if peer.activeSession == nil {
				delete(c.remotePeers, peerID)
			}
		}
	}
	if changed {
		c.markPeerGraphDirty(topologyChanged)
		c.logger.Info("authenticated remote peers changed", "count", c.authenticatedRemotePeerCount())
	}
	if snapshotChanged {
		c.publishStateChange()
	}
}

func authenticatedDirectPath(peer *RemotePeer) (Path, bool) {
	if peer == nil || peer.activeSession == nil || !peer.activeSession.authenticated || !peer.activeSession.path.IsDirect() {
		return Path{}, false
	}
	return peer.activeSession.path, true
}

func (c *Client) remotePeerSnapshots() []RemotePeerSnapshot {
	remotePeers := make([]RemotePeerSnapshot, 0, len(c.remotePeers))
	for _, peer := range c.remotePeers {
		activeSession := peer.activeSession
		if activeSession == nil || !activeSession.everAuthenticated {
			continue
		}
		transport := "direct"
		if !activeSession.path.IsDirect() {
			transport = "bridge"
		}
		remotePeers = append(remotePeers, RemotePeerSnapshot{
			PeerID:         peer.peerID,
			Address:        activeSession.path.Address().String(),
			SessionID:      hex.EncodeToString(activeSession.localHello.SessionID[:]),
			RTTMillis:      activeSession.rttMillis,
			Transport:      transport,
			Connected:      activeSession.authenticated,
			Nickname:       peer.memberState.nickname,
			Muted:          peer.memberState.muted,
			PlaybackMuted:  peer.memberState.playbackMuted,
			ScreenSharing:  peer.screenState.active(),
			ScreenStreamID: hex.EncodeToString(peer.screenState.streamID[:]),
		})
	}
	sort.Slice(remotePeers, func(i, j int) bool {
		return bytes.Compare(remotePeers[i].PeerID[:], remotePeers[j].PeerID[:]) < 0
	})
	return remotePeers
}

func (c *Client) authenticatedRemotePeerCount() int {
	count := 0
	for _, peer := range c.remotePeers {
		if peer.activeSession != nil && peer.activeSession.authenticated {
			count++
		}
	}
	return count
}
