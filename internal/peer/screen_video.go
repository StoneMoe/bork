package peer

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"time"

	"bork/internal/protocol"
)

const (
	ScreenVideoCodecH264Baseline = "avc1.42E01F"
	ScreenVideoCodecH264Main     = "avc1.4D401F"

	MaxScreenVideoChunkBytes = 256 << 10
	MaxScreenVideoWidth      = 1280
	MaxScreenVideoHeight     = 720

	reliableChannelScreenState         = 4
	screenStateVersion                 = 1
	screenStateSize                    = 31
	screenVideoFragmentVersion         = 1
	screenVideoFragmentHeaderSize      = 35
	maxScreenVideoFragmentBytes        = protocol.MaxRoomDatagramPayload - screenVideoFragmentHeaderSize
	maxScreenVideoFragments            = (MaxScreenVideoChunkBytes + maxScreenVideoFragmentBytes - 1) / maxScreenVideoFragmentBytes
	maxScreenVideoDurationMicros       = 1_000_000
	maxScreenVideoTimestamp            = (1 << 53) - 1
	screenVideoChunkTTL                = 750 * time.Millisecond
	maxScreenVideoRetainedBytes        = 4 << 20
	maxCompletedScreenVideoChunks      = 4
	screenVideoFlagKeyFrame       byte = 1
)

type screenState struct {
	generation uint64
	active     bool
	streamID   [16]byte
	codec      string
	width      uint16
	height     uint16
}

type screenCommandKind byte

const (
	screenCommandStart screenCommandKind = iota + 1
	screenCommandSend
	screenCommandStop
)

type screenCommand struct {
	kind      screenCommandKind
	codec     string
	width     uint16
	height    uint16
	timestamp uint64
	duration  uint32
	keyFrame  bool
	bytes     []byte
	result    chan screenCommandResult
}

type screenCommandResult struct {
	sent bool
	err  error
}

type ScreenVideoChunk struct {
	PeerID     string
	SessionID  [16]byte
	Generation uint64
	StreamID   [16]byte
	ChunkID    uint32
	Codec      string
	Width      uint16
	Height     uint16
	Timestamp  uint64
	Duration   uint32
	KeyFrame   bool
	Bytes      []byte
}

type screenVideoMetadata struct {
	generation uint64
	codec      string
	width      uint16
	height     uint16
	timestamp  uint64
	duration   uint32
	keyFrame   bool
}

type decodedScreenVideoFragment struct {
	metadata  screenVideoMetadata
	index     uint16
	count     uint16
	totalSize uint32
	bytes     []byte
}

type retainedScreenVideoFragment struct {
	bytes  []byte
	packet []byte
}

type screenVideoAssembly struct {
	chunkID       uint32
	metadata      screenVideoMetadata
	fragmentCount uint16
	totalSize     uint32
	fragments     map[uint16]retainedScreenVideoFragment
	videoBytes    int
	retainedBytes int
	startedAt     time.Time
}

type screenVideoReceiveState struct {
	newestChunkID    uint32
	deliveredChunkID uint32
	assemblies       map[uint32]*screenVideoAssembly
}

type completedScreenVideoChunk struct {
	chunkID  uint32
	metadata screenVideoMetadata
	bytes    []byte
	packets  [][]byte
	deadline time.Time
}

func encodeScreenState(state screenState) ([]byte, error) {
	if err := validateScreenState(state); err != nil {
		return nil, err
	}
	payload := make([]byte, screenStateSize)
	payload[0] = screenStateVersion
	binary.BigEndian.PutUint64(payload[1:9], state.generation)
	if state.active {
		payload[9] = 1
		payload[10], _ = screenVideoCodecCode(state.codec)
		binary.BigEndian.PutUint16(payload[11:13], state.width)
		binary.BigEndian.PutUint16(payload[13:15], state.height)
	}
	copy(payload[15:], state.streamID[:])
	return payload, nil
}

func decodeScreenState(payload []byte) (screenState, error) {
	if len(payload) != screenStateSize || payload[0] != screenStateVersion || payload[9] > 1 {
		return screenState{}, errors.New("screen state encoding is invalid")
	}
	state := screenState{
		generation: binary.BigEndian.Uint64(payload[1:9]),
		active:     payload[9] == 1,
		width:      binary.BigEndian.Uint16(payload[11:13]),
		height:     binary.BigEndian.Uint16(payload[13:15]),
	}
	copy(state.streamID[:], payload[15:])
	if state.active {
		var ok bool
		state.codec, ok = screenVideoCodecName(payload[10])
		if !ok {
			return screenState{}, errors.New("screen state codec is invalid")
		}
	} else if payload[10] != 0 {
		return screenState{}, errors.New("inactive screen state codec is not zero")
	}
	if err := validateScreenState(state); err != nil {
		return screenState{}, err
	}
	return state, nil
}

func validateScreenState(state screenState) error {
	if state.generation == 0 {
		return errors.New("screen state generation is zero")
	}
	if !state.active {
		if state.streamID != ([16]byte{}) || state.codec != "" || state.width != 0 || state.height != 0 {
			return errors.New("inactive screen state fields are not zero")
		}
		return nil
	}
	if state.streamID == ([16]byte{}) {
		return errors.New("active screen state stream is zero")
	}
	return validateScreenVideoConfig(state.codec, int(state.width), int(state.height))
}

func validateScreenVideoConfig(codec string, width, height int) error {
	if _, ok := screenVideoCodecCode(codec); !ok {
		return errors.New("screen video codec is not supported")
	}
	if width < 2 || height < 2 || width > MaxScreenVideoWidth || height > MaxScreenVideoHeight || width%2 != 0 || height%2 != 0 {
		return fmt.Errorf("screen video dimensions must be even and at most %dx%d", MaxScreenVideoWidth, MaxScreenVideoHeight)
	}
	return nil
}

func screenVideoCodecCode(codec string) (byte, bool) {
	switch codec {
	case ScreenVideoCodecH264Baseline:
		return 1, true
	case ScreenVideoCodecH264Main:
		return 2, true
	default:
		return 0, false
	}
}

func screenVideoCodecName(code byte) (string, bool) {
	switch code {
	case 1:
		return ScreenVideoCodecH264Baseline, true
	case 2:
		return ScreenVideoCodecH264Main, true
	default:
		return "", false
	}
}

func validateScreenVideoTiming(timestamp uint64, duration uint32) error {
	if timestamp > maxScreenVideoTimestamp || duration == 0 || duration > maxScreenVideoDurationMicros || timestamp > maxScreenVideoTimestamp-uint64(duration) {
		return errors.New("screen video timestamp or duration is invalid")
	}
	return nil
}

func validateScreenVideoMetadata(metadata screenVideoMetadata) error {
	if metadata.generation == 0 {
		return errors.New("screen video generation is zero")
	}
	if err := validateScreenVideoConfig(metadata.codec, int(metadata.width), int(metadata.height)); err != nil {
		return err
	}
	return validateScreenVideoTiming(metadata.timestamp, metadata.duration)
}

func encodeScreenVideoFragments(metadata screenVideoMetadata, data []byte) ([][]byte, error) {
	if err := validateScreenVideoMetadata(metadata); err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > MaxScreenVideoChunkBytes {
		return nil, fmt.Errorf("screen video chunk must contain 1 to %d bytes", MaxScreenVideoChunkBytes)
	}
	count := (len(data) + maxScreenVideoFragmentBytes - 1) / maxScreenVideoFragmentBytes
	if count > maxScreenVideoFragments {
		return nil, errors.New("screen video chunk needs too many fragments")
	}
	codec, _ := screenVideoCodecCode(metadata.codec)
	fragments := make([][]byte, count)
	for index := range count {
		start := index * maxScreenVideoFragmentBytes
		end := min(start+maxScreenVideoFragmentBytes, len(data))
		payload := make([]byte, screenVideoFragmentHeaderSize+end-start)
		payload[0] = screenVideoFragmentVersion
		if metadata.keyFrame {
			payload[1] = screenVideoFlagKeyFrame
		}
		payload[2] = codec
		binary.BigEndian.PutUint16(payload[3:5], uint16(index))
		binary.BigEndian.PutUint16(payload[5:7], uint16(count))
		binary.BigEndian.PutUint64(payload[7:15], metadata.generation)
		binary.BigEndian.PutUint64(payload[15:23], metadata.timestamp)
		binary.BigEndian.PutUint32(payload[23:27], metadata.duration)
		binary.BigEndian.PutUint32(payload[27:31], uint32(len(data)))
		binary.BigEndian.PutUint16(payload[31:33], metadata.width)
		binary.BigEndian.PutUint16(payload[33:35], metadata.height)
		copy(payload[screenVideoFragmentHeaderSize:], data[start:end])
		fragments[index] = payload
	}
	return fragments, nil
}

func decodeScreenVideoFragment(payload []byte) (decodedScreenVideoFragment, error) {
	if len(payload) <= screenVideoFragmentHeaderSize || len(payload) > protocol.MaxRoomDatagramPayload || payload[0] != screenVideoFragmentVersion || payload[1]&^screenVideoFlagKeyFrame != 0 {
		return decodedScreenVideoFragment{}, errors.New("screen video fragment encoding is invalid")
	}
	codec, ok := screenVideoCodecName(payload[2])
	if !ok {
		return decodedScreenVideoFragment{}, errors.New("screen video fragment codec is invalid")
	}
	fragment := decodedScreenVideoFragment{
		metadata: screenVideoMetadata{
			generation: binary.BigEndian.Uint64(payload[7:15]),
			codec:      codec,
			width:      binary.BigEndian.Uint16(payload[31:33]),
			height:     binary.BigEndian.Uint16(payload[33:35]),
			timestamp:  binary.BigEndian.Uint64(payload[15:23]),
			duration:   binary.BigEndian.Uint32(payload[23:27]),
			keyFrame:   payload[1]&screenVideoFlagKeyFrame != 0,
		},
		index:     binary.BigEndian.Uint16(payload[3:5]),
		count:     binary.BigEndian.Uint16(payload[5:7]),
		totalSize: binary.BigEndian.Uint32(payload[27:31]),
		bytes:     payload[screenVideoFragmentHeaderSize:],
	}
	if err := validateScreenVideoMetadata(fragment.metadata); err != nil {
		return decodedScreenVideoFragment{}, err
	}
	if fragment.totalSize == 0 || fragment.totalSize > MaxScreenVideoChunkBytes {
		return decodedScreenVideoFragment{}, errors.New("screen video fragment total size is invalid")
	}
	expectedCount := (int(fragment.totalSize) + maxScreenVideoFragmentBytes - 1) / maxScreenVideoFragmentBytes
	if fragment.count == 0 || int(fragment.count) != expectedCount || int(fragment.count) > maxScreenVideoFragments || fragment.index >= fragment.count {
		return decodedScreenVideoFragment{}, errors.New("screen video fragment count is invalid")
	}
	expectedBytes := maxScreenVideoFragmentBytes
	if fragment.index == fragment.count-1 {
		expectedBytes = int(fragment.totalSize) - int(fragment.index)*maxScreenVideoFragmentBytes
	}
	if len(fragment.bytes) != expectedBytes {
		return decodedScreenVideoFragment{}, errors.New("screen video fragment length is not canonical")
	}
	return fragment, nil
}

func screenVideoFragmentMatchesState(fragment decodedScreenVideoFragment, state screenState) bool {
	return state.active && fragment.metadata.generation == state.generation && fragment.metadata.codec == state.codec && fragment.metadata.width == state.width && fragment.metadata.height == state.height
}

func (c *Client) ScreenVideoChunks() <-chan ScreenVideoChunk { return c.screenVideoChunks }

func (c *Client) StartScreenShare(codec string, width, height int) error {
	if err := validateScreenVideoConfig(codec, width, height); err != nil {
		return err
	}
	return c.requestScreenCommand(screenCommand{
		kind: screenCommandStart, codec: codec, width: uint16(width), height: uint16(height), result: make(chan screenCommandResult, 1),
	}).err
}

func (c *Client) SendScreenVideoChunk(timestamp uint64, duration uint32, keyFrame bool, data []byte) (bool, error) {
	if err := validateScreenVideoTiming(timestamp, duration); err != nil {
		return false, err
	}
	if len(data) == 0 || len(data) > MaxScreenVideoChunkBytes {
		return false, errors.New("screen video chunk size is invalid")
	}
	result := c.requestScreenCommand(screenCommand{
		kind: screenCommandSend, timestamp: timestamp, duration: duration, keyFrame: keyFrame, bytes: data, result: make(chan screenCommandResult, 1),
	})
	return result.sent, result.err
}

func (c *Client) StopScreenShare() error {
	return c.requestScreenCommand(screenCommand{kind: screenCommandStop, result: make(chan screenCommandResult, 1)}).err
}

func (c *Client) requestScreenCommand(command screenCommand) screenCommandResult {
	if !c.started.Load() {
		return screenCommandResult{err: errors.New("peer client is not running")}
	}
	select {
	case <-c.loopReady:
	case <-c.loopDone:
		return screenCommandResult{err: errors.New("peer client is not running")}
	}
	select {
	case c.screenCommands <- command:
	case <-c.loopDone:
		return screenCommandResult{err: errors.New("peer client is not running")}
	}
	select {
	case result := <-command.result:
		return result
	case <-c.loopDone:
		return screenCommandResult{err: errors.New("peer client stopped")}
	}
}

func (c *Client) handleScreenCommand(command screenCommand) {
	var result screenCommandResult
	switch command.kind {
	case screenCommandStart:
		result.err = c.startScreenShare(command.codec, command.width, command.height)
	case screenCommandSend:
		result.sent, result.err = c.sendScreenVideoChunk(command.timestamp, command.duration, command.keyFrame, command.bytes)
	case screenCommandStop:
		result.err = c.stopScreenShare()
	default:
		result.err = errors.New("screen command is invalid")
	}
	command.result <- result
}

func (c *Client) startScreenShare(codec string, width, height uint16) error {
	if c.localScreenState.active {
		return errors.New("screen sharing is already active")
	}
	if c.localScreenState.generation == math.MaxUint64 {
		return errors.New("screen state generation is exhausted")
	}
	if err := validateScreenVideoConfig(codec, int(width), int(height)); err != nil {
		return err
	}
	var streamID [16]byte
	for streamID == ([16]byte{}) {
		if _, err := rand.Read(streamID[:]); err != nil {
			return fmt.Errorf("create screen video stream: %w", err)
		}
	}
	c.localScreenState = screenState{
		generation: c.localScreenState.generation + 1,
		active:     true,
		streamID:   streamID,
		codec:      codec,
		width:      width,
		height:     height,
	}
	c.screenVideoSendSequence = 0
	c.screenVideoChunkID = 0
	c.queueScreenStates()
	c.publishStateChange()
	return nil
}

func (c *Client) stopScreenShare() error {
	if !c.localScreenState.active {
		return errors.New("screen sharing is not active")
	}
	if c.localScreenState.generation == math.MaxUint64 {
		return errors.New("screen state generation is exhausted")
	}
	c.localScreenState = screenState{generation: c.localScreenState.generation + 1}
	c.screenVideoSendSequence = 0
	c.screenVideoChunkID = 0
	c.queueScreenStates()
	c.publishStateChange()
	return nil
}

func (c *Client) queueScreenStates() {
	payload, err := encodeScreenState(c.localScreenState)
	if err != nil {
		return
	}
	for _, peer := range c.remotePeers {
		session := peer.session
		if session == nil || !session.authenticated || session.reliable == nil || session.screenStateSentGeneration == c.localScreenState.generation {
			continue
		}
		if session.reliable.queue(reliableChannelScreenState, false, payload) != nil {
			continue
		}
		session.screenStateSentGeneration = c.localScreenState.generation
	}
}

func (c *Client) screenStateReady(session *PeeringSession) bool {
	return session != nil && session.authenticated && session.reliable != nil && session.screenStateSentGeneration == c.localScreenState.generation && !session.reliable.pendingChannel(reliableChannelScreenState)
}

func (c *Client) handleScreenState(sender *RemotePeer, payload []byte) {
	state, err := decodeScreenState(payload)
	if err != nil || sender == nil || sender.session == nil || state.generation <= sender.session.remoteScreenState.generation {
		return
	}
	sender.session.remoteScreenState = state
	c.removeScreenVideoAssembliesForSender(rawPeerIdentity(sender.identity))
	c.publishStateChange()
}

func (c *Client) sendScreenVideoChunk(timestamp uint64, duration uint32, keyFrame bool, data []byte) (bool, error) {
	if !c.localScreenState.active {
		return false, errors.New("screen sharing is not active")
	}
	metadata := screenVideoMetadata{
		generation: c.localScreenState.generation,
		codec:      c.localScreenState.codec,
		width:      c.localScreenState.width,
		height:     c.localScreenState.height,
		timestamp:  timestamp,
		duration:   duration,
		keyFrame:   keyFrame,
	}
	fragments, err := encodeScreenVideoFragments(metadata, data)
	if err != nil {
		return false, err
	}
	c.queueScreenStates()
	if c.screenVideoChunkID == math.MaxUint32 || uint64(len(fragments)) > math.MaxUint64-c.screenVideoSendSequence {
		return false, errors.New("screen video stream sequence is exhausted")
	}
	c.screenVideoChunkID++
	packets := make([][]byte, 0, len(fragments))
	for _, fragment := range fragments {
		c.screenVideoSendSequence++
		header := protocol.RoomDatagramHeader{
			Class: protocol.TrafficInteractive, SenderID: c.roomDatagramSenderID,
			StreamID: c.localScreenState.streamID, Sequence: c.screenVideoSendSequence,
		}
		packet, marshalErr := protocol.MarshalRoomDatagram(c.roomTag, header, c.screenVideoChunkID, fragment, c.roomDatagramProtector, c.localIdentity)
		if marshalErr != nil {
			return false, marshalErr
		}
		packets = append(packets, packet)
	}
	now := time.Now()
	c.refreshFanout(now)
	destinations := c.fanout.destinations
	if !c.fanoutReady(now) {
		destinations = make([]string, 0, len(c.remotePeers))
		for peerID, peer := range c.remotePeers {
			if peer.session != nil && peer.session.authenticated && peer.session.path.IsDirect() {
				destinations = append(destinations, peerID)
			}
		}
		sort.Strings(destinations)
	}
	ready := make([]string, 0, len(destinations))
	for _, peerID := range destinations {
		if peer := c.remotePeers[peerID]; peer != nil && c.screenStateReady(peer.session) {
			ready = append(ready, peerID)
		}
	}
	if len(ready) == 0 {
		return false, nil
	}
	return c.sendRealtimePacketsToPeers(protocol.TrafficInteractive, packets, ready, now.Add(screenVideoChunkTTL), 0), nil
}

func (c *Client) acceptScreenVideoFragment(key roomDatagramStreamKey, chunkID uint32, fragment decodedScreenVideoFragment, packet []byte, now time.Time) *completedScreenVideoChunk {
	state := c.screenVideoReceivers[key]
	if state == nil {
		state = &screenVideoReceiveState{assemblies: make(map[uint32]*screenVideoAssembly)}
		c.screenVideoReceivers[key] = state
	}
	if chunkID <= state.deliveredChunkID || (chunkID < state.newestChunkID && state.newestChunkID-chunkID > 1) {
		return nil
	}
	if chunkID > state.newestChunkID {
		state.newestChunkID = chunkID
		for retainedID := range state.assemblies {
			if retainedID < chunkID && chunkID-retainedID > 1 {
				c.removeScreenVideoChunkAssembly(key, retainedID)
			}
		}
	}
	assembly := state.assemblies[chunkID]
	if assembly == nil {
		assembly = &screenVideoAssembly{
			chunkID: chunkID, metadata: fragment.metadata, fragmentCount: fragment.count, totalSize: fragment.totalSize,
			fragments: make(map[uint16]retainedScreenVideoFragment), startedAt: now,
		}
		state.assemblies[chunkID] = assembly
	}
	if !now.Before(assembly.startedAt.Add(screenVideoChunkTTL)) {
		c.removeScreenVideoChunkAssembly(key, chunkID)
		return nil
	}
	if assembly.fragmentCount != fragment.count || assembly.totalSize != fragment.totalSize || assembly.metadata != fragment.metadata {
		c.removeScreenVideoChunkAssembly(key, chunkID)
		return nil
	}
	if _, duplicate := assembly.fragments[fragment.index]; duplicate {
		return nil
	}
	if len(fragment.bytes) > int(fragment.totalSize)-assembly.videoBytes {
		c.removeScreenVideoChunkAssembly(key, chunkID)
		return nil
	}
	cost := len(fragment.bytes) + len(packet)
	if !c.makeScreenVideoRetentionRoom(assembly, cost) || state.assemblies[chunkID] != assembly {
		return nil
	}
	retained := retainedScreenVideoFragment{
		bytes: append([]byte(nil), fragment.bytes...), packet: append([]byte(nil), packet...),
	}
	assembly.fragments[fragment.index] = retained
	assembly.videoBytes += len(retained.bytes)
	assembly.retainedBytes += cost
	c.screenVideoRetainedBytes += cost
	if len(assembly.fragments) != int(assembly.fragmentCount) {
		return nil
	}

	completed := &completedScreenVideoChunk{
		chunkID:  assembly.chunkID,
		metadata: assembly.metadata,
		bytes:    make([]byte, 0, assembly.videoBytes),
		packets:  make([][]byte, assembly.fragmentCount),
		deadline: assembly.startedAt.Add(screenVideoChunkTTL),
	}
	for index := range assembly.fragmentCount {
		part := assembly.fragments[index]
		completed.bytes = append(completed.bytes, part.bytes...)
		completed.packets[index] = part.packet
	}
	state.deliveredChunkID = chunkID
	for retainedID := range state.assemblies {
		if retainedID <= state.deliveredChunkID {
			c.removeScreenVideoChunkAssembly(key, retainedID)
		}
	}
	if len(completed.bytes) != int(assembly.totalSize) {
		return nil
	}
	return completed
}

func (c *Client) makeScreenVideoRetentionRoom(current *screenVideoAssembly, cost int) bool {
	if cost <= 0 || cost > maxScreenVideoRetainedBytes {
		return false
	}
	for cost > maxScreenVideoRetainedBytes-c.screenVideoRetainedBytes {
		var oldestKey roomDatagramStreamKey
		var oldestChunkID uint32
		var oldestAt time.Time
		found := false
		for key, state := range c.screenVideoReceivers {
			for chunkID, assembly := range state.assemblies {
				if assembly == current {
					continue
				}
				if !found || assembly.startedAt.Before(oldestAt) {
					oldestKey, oldestChunkID, oldestAt, found = key, chunkID, assembly.startedAt, true
				}
			}
		}
		if !found {
			return false
		}
		c.removeScreenVideoChunkAssembly(oldestKey, oldestChunkID)
	}
	return true
}

func (c *Client) removeScreenVideoAssembly(key roomDatagramStreamKey) {
	state := c.screenVideoReceivers[key]
	if state == nil {
		return
	}
	for chunkID := range state.assemblies {
		c.removeScreenVideoChunkAssembly(key, chunkID)
	}
	state.assemblies = nil
}

func (c *Client) removeScreenVideoChunkAssembly(key roomDatagramStreamKey, chunkID uint32) {
	state := c.screenVideoReceivers[key]
	if state == nil || state.assemblies[chunkID] == nil {
		return
	}
	c.screenVideoRetainedBytes -= state.assemblies[chunkID].retainedBytes
	delete(state.assemblies, chunkID)
}

func (c *Client) removeScreenVideoAssembliesForSender(sender [32]byte) {
	for key := range c.screenVideoReceivers {
		if key.sender == sender {
			c.removeScreenVideoAssembly(key)
			delete(c.screenVideoReceivers, key)
		}
	}
}

func (c *Client) expireScreenVideoChunks(now time.Time) {
	for key, state := range c.screenVideoReceivers {
		for chunkID, assembly := range state.assemblies {
			if !now.Before(assembly.startedAt.Add(screenVideoChunkTTL)) {
				c.removeScreenVideoChunkAssembly(key, chunkID)
			}
		}
	}
}

func (c *Client) forwardScreenVideoChunk(senderID string, source netip.AddrPort, packets [][]byte, deadline time.Time) {
	if c.roomNetwork == nil {
		return
	}
	sender := c.remotePeers[senderID]
	if sender == nil || sender.session == nil || !sender.session.authenticated || !sender.session.path.IsDirect() || sender.session.path.Address() != source {
		return
	}
	assignment := sender.session.inboundFanout
	if assignment.generation == 0 {
		return
	}
	c.sendRealtimePacketsToPeers(protocol.TrafficInteractive, packets, assignment.listeners, deadline, 0)
}

func (c *Client) deliverScreenVideoChunk(chunk ScreenVideoChunk) {
	select {
	case c.screenVideoChunks <- chunk:
	default:
		select {
		case <-c.screenVideoChunks:
		default:
		}
		select {
		case c.screenVideoChunks <- chunk:
		default:
		}
	}
}
