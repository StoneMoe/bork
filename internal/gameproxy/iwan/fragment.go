package iwan

import (
	"encoding/binary"
	"time"
)

const (
	fragmentOffsetMask = uint32(0x1fff)
	fragmentLengthMask = uint32(0x7ff)
	fragmentKnownBits  = uint32(1) | fragmentOffsetMask<<2 | fragmentLengthMask<<15
)

type Reassembler struct {
	slots [FragmentSlots]fragmentSlot
}

type fragmentSlot struct {
	data      [fragmentOutputSize]byte
	received  [fragmentOutputSize / 8]byte
	id        uint32
	updatedAt time.Time
	finalSize int
	inUse     bool
	hasFinal  bool
}

func NewReassembler() *Reassembler {
	return &Reassembler{}
}

func (reassembler *Reassembler) Push(packet []byte, session Session, now time.Time) ([]byte, bool, error) {
	header, id, offset, final, fragment, err := parseFragment(packet)
	if err != nil {
		return nil, false, err
	}
	if header.token != session.Token || header.session != session.ID {
		return nil, false, ErrSessionMismatch
	}
	slot, err := reassembler.slotFor(id, now)
	if err != nil {
		return nil, false, err
	}
	end := offset + len(fragment)
	if end > fragmentOutputSize {
		slot.clear()
		return nil, false, ErrFragmentOverflow
	}
	if final && slot.hasFinal && slot.finalSize != end {
		slot.clear()
		return nil, false, ErrFragmentConflict
	}
	if final {
		for index := end; index < fragmentOutputSize; index++ {
			if slot.hasByte(index) {
				slot.clear()
				return nil, false, ErrFragmentConflict
			}
		}
	}
	if slot.hasFinal && end > slot.finalSize {
		slot.clear()
		return nil, false, ErrFragmentConflict
	}
	for index := offset; index < end; index++ {
		if slot.hasByte(index) {
			slot.clear()
			return nil, false, ErrFragmentOverlap
		}
	}
	copy(slot.data[offset:end], fragment)
	for index := offset; index < end; index++ {
		slot.markByte(index)
	}
	slot.updatedAt = now
	if final {
		slot.hasFinal = true
		slot.finalSize = end
	}
	if !slot.hasFinal || !slot.complete() {
		return nil, false, nil
	}
	payload := append([]byte(nil), slot.data[:slot.finalSize]...)
	slot.clear()
	xorBytes(session.xorKey, payload)
	return payload, true, nil
}

func parseFragment(packet []byte) (wireHeader, uint32, int, bool, []byte, error) {
	if len(packet) < fragmentHeaderSize+1 {
		return wireHeader{}, 0, 0, false, nil, ErrMalformedPacket
	}
	header, err := parseHeader(packet)
	if err != nil {
		return wireHeader{}, 0, 0, false, nil, err
	}
	if header.typ != TypeIPFragment || header.flags != 0 {
		return wireHeader{}, 0, 0, false, nil, ErrUnknownPacketType
	}
	id := binary.LittleEndian.Uint32(packet[16:20])
	bits := binary.LittleEndian.Uint32(packet[20:24])
	length := int((bits >> 15) & fragmentLengthMask)
	if id == 0 || bits&^fragmentKnownBits != 0 || length == 0 || len(packet) != fragmentHeaderSize+length {
		return wireHeader{}, 0, 0, false, nil, ErrMalformedPacket
	}
	offset := int((bits >> 2) & fragmentOffsetMask)
	return header, id, offset, bits&1 != 0, packet[fragmentHeaderSize:], nil
}

func (reassembler *Reassembler) slotFor(id uint32, now time.Time) (*fragmentSlot, error) {
	var available *fragmentSlot
	for index := range reassembler.slots {
		slot := &reassembler.slots[index]
		if slot.inUse && now.Sub(slot.updatedAt) >= FragmentTimeout {
			slot.clear()
		}
		if slot.inUse && slot.id == id {
			return slot, nil
		}
		if !slot.inUse && available == nil {
			available = slot
		}
	}
	if available == nil {
		return nil, ErrFragmentSlotsFull
	}
	available.inUse = true
	available.id = id
	available.updatedAt = now
	return available, nil
}

func (slot *fragmentSlot) hasByte(index int) bool {
	return slot.received[index/8]&(1<<uint(index%8)) != 0
}

func (slot *fragmentSlot) markByte(index int) {
	slot.received[index/8] |= 1 << uint(index%8)
}

func (slot *fragmentSlot) complete() bool {
	for index := 0; index < slot.finalSize; index++ {
		if !slot.hasByte(index) {
			return false
		}
	}
	return true
}

func (slot *fragmentSlot) clear() {
	*slot = fragmentSlot{}
}
