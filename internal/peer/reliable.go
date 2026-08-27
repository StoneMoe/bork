package peer

import (
	"errors"
	"time"

	"bork/internal/protocol"
)

const (
	reliableChannelTopology    uint16 = 1
	reliableChannelFanout             = 2
	reliableChannelMemberState        = 3
	reliableChannelScreenState        = 4
	reliableChannelFileControl        = 5
	reliableChannelFileData           = 6

	reliableMSS                = protocol.MaxReliablePayload
	initialReliableCwnd        = 4 * reliableMSS
	minimumReliableCwnd        = 2 * reliableMSS
	maximumReliableCwnd        = 64 * reliableMSS
	initialReliableRTO         = 500 * time.Millisecond
	minimumReliableRTO         = 100 * time.Millisecond
	maximumReliableRTO         = 5 * time.Second
	maxQueuedReliableBytes     = 4 << 20
	maxReassemblyReliableBytes = 4 << 20
	maxReliableAssemblies      = 4096
	maxReliableFragments       = 8192
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
	dataCursor      int
	queuedBytes     int
	bytesInFlight   int
	reassemblyBytes int
	reassemblyCount int
	reassemblyParts int
	cwnd            int
	ssthresh        int
	rto             time.Duration
	srtt            time.Duration
	rttvar          time.Duration
	haveRTT         bool
	preferACK       bool
}

type reliableChannel struct {
	nextFragmentSequence  uint64
	received              sequenceWindow
	ackDirty              bool
	nextDeliveredFragment uint64
	reassemblies          map[uint64]*reliableAssembly
}

type reliableFragment struct {
	channel          uint16
	fragmentSequence uint64
	fragmentIndex    uint16
	fragmentCount    uint16
	payload          []byte
	sentAt           time.Time
	transmissions    int
	timeoutPending   bool
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
	kind      reliableReservationKind
	fragment  *reliableFragment
	channel   *reliableChannel
	ackIndex  int
	dataIndex int
	now       time.Time
}

func newReliableTransport() *reliableTransport {
	return &reliableTransport{
		channels:  make(map[uint16]*reliableChannel),
		cwnd:      initialReliableCwnd,
		ssthresh:  maximumReliableCwnd,
		rto:       initialReliableRTO,
		preferACK: true,
	}
}

func (r *reliableTransport) queue(channel uint16, payload []byte) error {
	if err := r.canQueue(channel, len(payload)); err != nil {
		return err
	}

	fragmentCount := (len(payload) + reliableMSS - 1) / reliableMSS
	state := r.channels[channel]
	if state == nil {
		state = r.addChannel(channel)
	}
	for index := 0; index < fragmentCount; index++ {
		start := index * reliableMSS
		end := min(start+reliableMSS, len(payload))
		state.nextFragmentSequence++
		r.outbound = append(r.outbound, &reliableFragment{
			channel:          channel,
			fragmentSequence: state.nextFragmentSequence,
			fragmentIndex:    uint16(index),
			fragmentCount:    uint16(fragmentCount),
			payload:          append([]byte(nil), payload[start:end]...),
		})
	}
	r.queuedBytes += len(payload)
	return nil
}

func (r *reliableTransport) canQueue(channel uint16, size int) error {
	if _, known := reliableChannelOrdered(channel); !known {
		return errors.New("reliable channel is unknown")
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
	if state != nil && uint64(fragmentCount) > ^uint64(0)-state.nextFragmentSequence {
		return errors.New("reliable sequence exhausted")
	}
	return nil
}

func (r *reliableTransport) nextSend(now time.Time) (protocol.ReliablePacket, reliableReservation, bool) {
	if r.preferACK {
		if packet, reservation, ok := r.nextAck(); ok {
			return packet, reservation, true
		}
		return r.nextPacket(now)
	}
	if packet, reservation, ok := r.nextPacket(now); ok {
		return packet, reservation, true
	}
	return r.nextAck()
}

func (r *reliableTransport) nextPacket(now time.Time) (protocol.ReliablePacket, reliableReservation, bool) {
	if fragment, index, ok := r.nextFragment(func(fragment *reliableFragment) bool {
		return fragment.transmissions != 0 && (fragment.timeoutPending || !now.Before(fragment.sentAt.Add(r.rto)))
	}); ok {
		return r.packetFor(fragment), r.fragmentReservation(reliableReservationRetransmit, fragment, index, now), true
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
	if fragment, index, ok := r.nextFragment(func(fragment *reliableFragment) bool {
		if fragment.transmissions != 0 || r.bytesInFlight+len(fragment.payload) > r.cwnd {
			return false
		}
		oldest := oldestTransmitted[fragment.channel]
		return oldest == 0 || fragment.fragmentSequence-oldest < 64
	}); ok {
		return r.packetFor(fragment), r.fragmentReservation(reliableReservationNew, fragment, index, now), true
	}
	return protocol.ReliablePacket{}, reliableReservation{}, false
}

func (r *reliableTransport) nextFragment(eligible func(*reliableFragment) bool) (*reliableFragment, int, bool) {
	for offset := 0; offset < len(r.ackChannels); offset++ {
		index := (r.dataCursor + offset) % len(r.ackChannels)
		channel := r.ackChannels[index]
		for _, fragment := range r.outbound {
			if fragment.channel == channel && eligible(fragment) {
				return fragment, index, true
			}
		}
	}
	return nil, 0, false
}

func (r *reliableTransport) receive(packet protocol.ReliablePacket, now time.Time) []deliveredReliableMessage {
	ordered, known := reliableChannelOrdered(packet.Channel)
	if !known {
		return nil
	}
	r.consumeFragmentAck(packet.Channel, packet.FragmentAckBase, packet.FragmentAckBitmap, now)
	if packet.AckOnly() {
		return nil
	}

	state := r.channels[packet.Channel]
	if state == nil {
		state = r.addChannel(packet.Channel)
	}
	// Rejected repeats still trigger the current ACK, but only retained data is
	// added to the receive window below.
	state.ackDirty = true
	if !state.received.mayAccept(packet.FragmentSequence) {
		return nil
	}
	messageStart := packet.FragmentSequence - uint64(packet.FragmentIndex)
	if ordered && (state.nextDeliveredFragment == 0 || messageStart < state.nextDeliveredFragment) {
		return nil
	}

	assembly := state.reassemblies[messageStart]
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
	if r.reassemblyParts >= maxReliableFragments {
		return nil
	}
	assembly = state.reassemblies[messageStart]
	if assembly == nil {
		if r.reassemblyCount >= maxReliableAssemblies {
			return nil
		}
		assembly = &reliableAssembly{
			fragmentCount: packet.FragmentCount,
			fragments:     make(map[uint16][]byte),
		}
		state.reassemblies[messageStart] = assembly
		r.reassemblyCount++
	}
	assembly.fragments[packet.FragmentIndex] = append([]byte(nil), packet.Payload...)
	assembly.bytes += len(packet.Payload)
	r.reassemblyBytes += len(packet.Payload)
	r.reassemblyParts++
	state.received.accept(packet.FragmentSequence)
	if len(assembly.fragments) != int(assembly.fragmentCount) {
		return nil
	}
	if !ordered {
		delivered := deliveredReliableMessage{channel: packet.Channel, payload: assembleReliable(assembly)}
		r.removeAssembly(state, messageStart)
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
				Channel:           channel,
				FragmentAckBase:   state.received.highest,
				FragmentAckBitmap: state.received.seen,
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

func (r *reliableTransport) discardOutboundChannel(channel uint16) {
	remaining := r.outbound[:0]
	for _, fragment := range r.outbound {
		if fragment.channel != channel {
			remaining = append(remaining, fragment)
			continue
		}
		r.queuedBytes -= len(fragment.payload)
		if fragment.transmissions != 0 {
			r.bytesInFlight -= len(fragment.payload)
		}
	}
	r.outbound = remaining
}

func (r *reliableTransport) discardInboundChannel(channel uint16) {
	state := r.channels[channel]
	if state == nil {
		return
	}
	for sequence := range state.reassemblies {
		r.removeAssembly(state, sequence)
	}
}

func (r *reliableTransport) addChannel(channel uint16) *reliableChannel {
	state := &reliableChannel{
		nextDeliveredFragment: 1,
		reassemblies:          make(map[uint64]*reliableAssembly),
	}
	r.channels[channel] = state
	r.ackChannels = append(r.ackChannels, channel)
	return state
}

func (r *reliableTransport) packetFor(fragment *reliableFragment) protocol.ReliablePacket {
	state := r.channels[fragment.channel]
	return protocol.ReliablePacket{
		Channel:           fragment.channel,
		FragmentSequence:  fragment.fragmentSequence,
		FragmentIndex:     fragment.fragmentIndex,
		FragmentCount:     fragment.fragmentCount,
		FragmentAckBase:   state.received.highest,
		FragmentAckBitmap: state.received.seen,
		Payload:           fragment.payload,
	}
}

func (r *reliableTransport) fragmentReservation(kind reliableReservationKind, fragment *reliableFragment, dataIndex int, now time.Time) reliableReservation {
	state := r.channels[fragment.channel]
	return reliableReservation{
		kind: kind, fragment: fragment, channel: state, dataIndex: dataIndex,
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
		if !reservation.fragment.timeoutPending {
			// Freeze every fragment already expired into this wave before changing the global RTO.
			for _, fragment := range r.outbound {
				if fragment.transmissions != 0 && !reservation.now.Before(fragment.sentAt.Add(r.rto)) {
					fragment.timeoutPending = true
				}
			}
			r.ssthresh = max(r.cwnd/2, minimumReliableCwnd)
			r.cwnd = minimumReliableCwnd
			r.rto = min(r.rto*2, maximumReliableRTO)
		}
		reservation.fragment.timeoutPending = false
		reservation.fragment.transmissions++
		reservation.fragment.sentAt = reservation.now
	case reliableReservationACK:
	default:
		return
	}
	r.advance(reservation)
	reservation.channel.ackDirty = false
}

func (r *reliableTransport) reject(reservation reliableReservation) {
	r.advance(reservation)
}

func (r *reliableTransport) advance(reservation reliableReservation) {
	if reservation.kind == reliableReservationACK {
		r.ackCursor = (reservation.ackIndex + 1) % len(r.ackChannels)
		r.preferACK = false
		return
	}
	r.dataCursor = (reservation.dataIndex + 1) % len(r.ackChannels)
	r.preferACK = true
}

func (r *reliableTransport) consumeFragmentAck(channel uint16, base, bitmap uint64, now time.Time) {
	if base == 0 || bitmap == 0 {
		return
	}
	ackedBytes := 0
	remaining := r.outbound[:0]
	for _, fragment := range r.outbound {
		if fragment.channel != channel || fragment.transmissions == 0 || !protocol.FragmentAckContains(base, bitmap, fragment.fragmentSequence) {
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
	r.reassemblyCount--
	r.reassemblyParts -= len(assembly.fragments)
	delete(state.reassemblies, sequence)
}

func (r *reliableTransport) deliverOrdered(channel uint16, state *reliableChannel) []deliveredReliableMessage {
	var delivered []deliveredReliableMessage
	for {
		assembly := state.reassemblies[state.nextDeliveredFragment]
		if assembly == nil || len(assembly.fragments) != int(assembly.fragmentCount) {
			return delivered
		}
		delivered = append(delivered, deliveredReliableMessage{
			channel: channel,
			payload: assembleReliable(assembly),
		})
		r.removeAssembly(state, state.nextDeliveredFragment)
		next := state.nextDeliveredFragment + uint64(assembly.fragmentCount)
		if next < state.nextDeliveredFragment {
			state.nextDeliveredFragment = 0
			return delivered
		}
		state.nextDeliveredFragment = next
	}
}

func reliableChannelOrdered(channel uint16) (ordered, known bool) {
	switch channel {
	case reliableChannelTopology, reliableChannelFanout, reliableChannelMemberState, reliableChannelScreenState, reliableChannelFileControl:
		return true, true
	case reliableChannelFileData:
		return false, true
	default:
		return false, false
	}
}

func assembleReliable(assembly *reliableAssembly) []byte {
	payload := make([]byte, 0, assembly.bytes)
	for index := uint16(0); index < assembly.fragmentCount; index++ {
		payload = append(payload, assembly.fragments[index]...)
	}
	return payload
}
