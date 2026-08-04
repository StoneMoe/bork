package peer

import (
	"errors"
	"time"

	"bork/internal/protocol"
)

const (
	reliableMSS                = protocol.MaxReliablePayload
	initialReliableCwnd        = 4 * reliableMSS
	minimumReliableCwnd        = 2 * reliableMSS
	maximumReliableCwnd        = 64 * reliableMSS
	initialReliableRTO         = 500 * time.Millisecond
	minimumReliableRTO         = 100 * time.Millisecond
	maximumReliableRTO         = 5 * time.Second
	maxQueuedReliableBytes     = 4 << 20
	maxReassemblyReliableBytes = 4 << 20
)

type deliveredReliableMessage struct {
	channel uint16
	payload []byte
}

type reliableTransport struct {
	channels        map[uint16]*reliableChannel
	outbound        []*reliableFragment
	ackChannels     []uint16
	ackCursor       int
	queuedBytes     int
	bytesInFlight   int
	reassemblyBytes int
	cwnd            int
	ssthresh        int
	rto             time.Duration
	srtt            time.Duration
	rttvar          time.Duration
	haveRTT         bool
	timeoutWave     bool
}

type reliableChannel struct {
	modeSet              bool
	ordered              bool
	nextMessageSequence  uint64
	nextFragmentSequence uint64
	received             sequenceWindow
	ackDirty             bool
	nextDeliveredMessage uint64
	reassemblies         map[uint64]*reliableAssembly
}

type reliableFragment struct {
	channel          uint16
	fragmentSequence uint64
	messageSequence  uint64
	fragmentIndex    uint16
	fragmentCount    uint16
	payload          []byte
	sentAt           time.Time
	transmissions    int
}

type reliableAssembly struct {
	fragmentCount uint16
	fragments     map[uint16][]byte
	bytes         int
}

type reliableReservationKind uint8

const (
	reliableReservationNew reliableReservationKind = iota + 1
	reliableReservationRetransmit
	reliableReservationACK
)

type reliableReservation struct {
	kind     reliableReservationKind
	fragment *reliableFragment
	channel  *reliableChannel
	ackIndex int
	now      time.Time
}

func newReliableTransport() *reliableTransport {
	return &reliableTransport{
		channels: make(map[uint16]*reliableChannel),
		cwnd:     initialReliableCwnd,
		ssthresh: maximumReliableCwnd,
		rto:      initialReliableRTO,
	}
}

func (r *reliableTransport) queue(channel uint16, ordered bool, payload []byte) error {
	if err := r.canQueue(channel, ordered, len(payload)); err != nil {
		return err
	}

	fragmentCount := (len(payload) + reliableMSS - 1) / reliableMSS
	state := r.channels[channel]
	if state == nil {
		state = r.addChannel(channel)
	}
	state.modeSet = true
	state.ordered = ordered
	state.nextMessageSequence++
	messageSequence := state.nextMessageSequence

	for index := 0; index < fragmentCount; index++ {
		start := index * reliableMSS
		end := min(start+reliableMSS, len(payload))
		state.nextFragmentSequence++
		r.outbound = append(r.outbound, &reliableFragment{
			channel:          channel,
			fragmentSequence: state.nextFragmentSequence,
			messageSequence:  messageSequence,
			fragmentIndex:    uint16(index),
			fragmentCount:    uint16(fragmentCount),
			payload:          append([]byte(nil), payload[start:end]...),
		})
	}
	r.queuedBytes += len(payload)
	return nil
}

func (r *reliableTransport) canQueue(channel uint16, ordered bool, size int) error {
	if channel == 0 {
		return errors.New("reliable channel is zero")
	}
	if size == 0 {
		return errors.New("reliable message is empty")
	}
	if size > protocol.MaxReliableFragments*reliableMSS {
		return errors.New("reliable message is too large")
	}
	if size > maxQueuedReliableBytes-r.queuedBytes {
		return errors.New("reliable queue is full")
	}

	fragmentCount := (size + reliableMSS - 1) / reliableMSS
	state := r.channels[channel]
	if state != nil && state.modeSet && state.ordered != ordered {
		return errors.New("reliable channel mode changed")
	}
	if state != nil && (state.nextMessageSequence == ^uint64(0) || uint64(fragmentCount) > ^uint64(0)-state.nextFragmentSequence) {
		return errors.New("reliable sequence exhausted")
	}
	return nil
}

func (r *reliableTransport) nextPacket(now time.Time) (protocol.ReliablePacket, reliableReservation, bool) {
	for _, fragment := range r.outbound {
		if fragment.transmissions == 0 || now.Before(fragment.sentAt.Add(r.rto)) {
			continue
		}
		return r.packetFor(fragment), r.fragmentReservation(reliableReservationRetransmit, fragment, now), true
	}
	oldestTransmitted := make(map[uint16]uint64)
	for _, fragment := range r.outbound {
		if fragment.transmissions == 0 {
			continue
		}
		oldest := oldestTransmitted[fragment.channel]
		if oldest == 0 || fragment.fragmentSequence < oldest {
			oldestTransmitted[fragment.channel] = fragment.fragmentSequence
		}
	}
	for _, fragment := range r.outbound {
		if fragment.transmissions != 0 || r.bytesInFlight+len(fragment.payload) > r.cwnd {
			continue
		}
		if oldest := oldestTransmitted[fragment.channel]; oldest != 0 && fragment.fragmentSequence-oldest >= 64 {
			continue
		}
		return r.packetFor(fragment), r.fragmentReservation(reliableReservationNew, fragment, now), true
	}
	return protocol.ReliablePacket{}, reliableReservation{}, false
}

func (r *reliableTransport) receive(packet protocol.ReliablePacket, now time.Time) []deliveredReliableMessage {
	r.consumeACK(packet.Channel, packet.AckBase, packet.AckBitmap, now)
	if packet.AckOnly() {
		return nil
	}

	state := r.channels[packet.Channel]
	ordered := packet.Ordered()
	if state != nil && state.modeSet && state.ordered != ordered {
		return nil
	}
	if state == nil {
		state = r.addChannel(packet.Channel)
	}
	// Rejected repeats still trigger the current ACK, but only retained data is
	// added to the receive window below.
	state.ackDirty = true
	if !state.received.mayAccept(packet.FragmentSequence) {
		return nil
	}
	if ordered && packet.MessageSequence < state.nextDeliveredMessage {
		return nil
	}

	assembly := state.reassemblies[packet.MessageSequence]
	if assembly != nil {
		if assembly.fragmentCount != packet.FragmentCount {
			return nil
		}
		if _, exists := assembly.fragments[packet.FragmentIndex]; exists {
			return nil
		}
	}
	if len(packet.Payload) > maxReassemblyReliableBytes-r.reassemblyBytes {
		return nil
	}
	assembly = state.reassemblies[packet.MessageSequence]
	if assembly == nil {
		assembly = &reliableAssembly{
			fragmentCount: packet.FragmentCount,
			fragments:     make(map[uint16][]byte),
		}
		state.reassemblies[packet.MessageSequence] = assembly
	}
	assembly.fragments[packet.FragmentIndex] = append([]byte(nil), packet.Payload...)
	assembly.bytes += len(packet.Payload)
	r.reassemblyBytes += len(packet.Payload)
	state.modeSet = true
	state.ordered = ordered
	state.received.accept(packet.FragmentSequence)
	if len(assembly.fragments) != int(assembly.fragmentCount) {
		return nil
	}
	if !ordered {
		delivered := deliveredReliableMessage{channel: packet.Channel, payload: assembleReliable(assembly)}
		r.removeAssembly(state, packet.MessageSequence)
		return []deliveredReliableMessage{delivered}
	}
	return r.deliverOrdered(packet.Channel, state)
}

func (r *reliableTransport) nextAck() (protocol.ReliablePacket, reliableReservation, bool) {
	for offset := 0; offset < len(r.ackChannels); offset++ {
		index := (r.ackCursor + offset) % len(r.ackChannels)
		channel := r.ackChannels[index]
		state := r.channels[channel]
		if !state.ackDirty {
			continue
		}
		return protocol.ReliablePacket{
				Channel:   channel,
				Flags:     protocol.ReliableFlagAckOnly,
				AckBase:   state.received.highest,
				AckBitmap: state.received.seen,
			}, reliableReservation{
				kind: reliableReservationACK, channel: state, ackIndex: index,
			}, true
	}
	return protocol.ReliablePacket{}, reliableReservation{}, false
}

func (r *reliableTransport) pendingChannel(channel uint16) bool {
	for _, fragment := range r.outbound {
		if fragment.channel == channel {
			return true
		}
	}
	return false
}

func (r *reliableTransport) addChannel(channel uint16) *reliableChannel {
	state := &reliableChannel{
		nextDeliveredMessage: 1,
		reassemblies:         make(map[uint64]*reliableAssembly),
	}
	r.channels[channel] = state
	r.ackChannels = append(r.ackChannels, channel)
	return state
}

func (r *reliableTransport) packetFor(fragment *reliableFragment) protocol.ReliablePacket {
	state := r.channels[fragment.channel]
	flags := byte(0)
	if state.ordered {
		flags = protocol.ReliableFlagOrdered
	}
	return protocol.ReliablePacket{
		Channel:          fragment.channel,
		Flags:            flags,
		FragmentSequence: fragment.fragmentSequence,
		MessageSequence:  fragment.messageSequence,
		FragmentIndex:    fragment.fragmentIndex,
		FragmentCount:    fragment.fragmentCount,
		AckBase:          state.received.highest,
		AckBitmap:        state.received.seen,
		Payload:          fragment.payload,
	}
}

func (r *reliableTransport) fragmentReservation(kind reliableReservationKind, fragment *reliableFragment, now time.Time) reliableReservation {
	state := r.channels[fragment.channel]
	return reliableReservation{
		kind: kind, fragment: fragment, channel: state,
		now: now,
	}
}

func (r *reliableTransport) commit(reservation reliableReservation) {
	switch reservation.kind {
	case reliableReservationNew:
		reservation.fragment.transmissions++
		reservation.fragment.sentAt = reservation.now
		r.bytesInFlight += len(reservation.fragment.payload)
	case reliableReservationRetransmit:
		if !r.timeoutWave {
			r.ssthresh = max(r.cwnd/2, minimumReliableCwnd)
			r.cwnd = minimumReliableCwnd
			r.rto = min(r.rto*2, maximumReliableRTO)
			r.timeoutWave = true
		}
		reservation.fragment.transmissions++
		reservation.fragment.sentAt = reservation.now
	case reliableReservationACK:
		r.ackCursor = (reservation.ackIndex + 1) % len(r.ackChannels)
	default:
		return
	}
	reservation.channel.ackDirty = false
}

func (r *reliableTransport) consumeACK(channel uint16, base, bitmap uint64, now time.Time) {
	if base == 0 || bitmap == 0 {
		return
	}
	ackedBytes := 0
	remaining := r.outbound[:0]
	for _, fragment := range r.outbound {
		if fragment.channel != channel || fragment.transmissions == 0 || !protocol.AckContains(base, bitmap, fragment.fragmentSequence) {
			remaining = append(remaining, fragment)
			continue
		}
		ackedBytes += len(fragment.payload)
		r.queuedBytes -= len(fragment.payload)
		r.bytesInFlight -= len(fragment.payload)
		if fragment.transmissions == 1 && !now.Before(fragment.sentAt) {
			r.updateRTT(now.Sub(fragment.sentAt))
		}
	}
	r.outbound = remaining
	if ackedBytes == 0 {
		return
	}
	r.timeoutWave = false
	if r.cwnd < r.ssthresh {
		r.cwnd += ackedBytes
	} else {
		increase := reliableMSS * ackedBytes / r.cwnd
		r.cwnd += max(increase, 1)
	}
	r.cwnd = min(r.cwnd, maximumReliableCwnd)
}

func (r *reliableTransport) updateRTT(sample time.Duration) {
	if !r.haveRTT {
		r.srtt = sample
		r.rttvar = sample / 2
		r.haveRTT = true
	} else {
		deviation := r.srtt - sample
		if deviation < 0 {
			deviation = -deviation
		}
		r.rttvar = (3*r.rttvar + deviation) / 4
		r.srtt = (7*r.srtt + sample) / 8
	}
	r.rto = min(max(r.srtt+4*r.rttvar, minimumReliableRTO), maximumReliableRTO)
}

func (r *reliableTransport) removeAssembly(state *reliableChannel, sequence uint64) {
	assembly := state.reassemblies[sequence]
	if assembly == nil {
		return
	}
	r.reassemblyBytes -= assembly.bytes
	delete(state.reassemblies, sequence)
}

func (r *reliableTransport) deliverOrdered(channel uint16, state *reliableChannel) []deliveredReliableMessage {
	var delivered []deliveredReliableMessage
	for {
		assembly := state.reassemblies[state.nextDeliveredMessage]
		if assembly == nil || len(assembly.fragments) != int(assembly.fragmentCount) {
			return delivered
		}
		delivered = append(delivered, deliveredReliableMessage{
			channel: channel,
			payload: assembleReliable(assembly),
		})
		r.removeAssembly(state, state.nextDeliveredMessage)
		state.nextDeliveredMessage++
	}
}

func assembleReliable(assembly *reliableAssembly) []byte {
	payload := make([]byte, 0, assembly.bytes)
	for index := uint16(0); index < assembly.fragmentCount; index++ {
		payload = append(payload, assembly.fragments[index]...)
	}
	return payload
}
