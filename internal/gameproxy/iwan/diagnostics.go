package iwan

import (
	"encoding/binary"
	"sync/atomic"
)

const (
	ipv4ProtocolICMP byte = 1
	ipv4ProtocolTCP  byte = 6
	ipv4ProtocolUDP  byte = 17

	icmpv4TypeDestinationUnreachable byte = 3
	icmpv4CodePortUnreachable        byte = 3
)

type DataPathStats struct {
	OutboundStackPackets                     uint64
	OutboundStackBytes                       uint64
	OutboundUDPPackets                       uint64
	OutboundNodeDatagrams                    uint64
	OutboundNodeBytes                        uint64
	InboundNodeDatagrams                     uint64
	InboundNodeBytes                         uint64
	InboundDataDatagrams                     uint64
	InboundDataBytes                         uint64
	InboundFragmentDatagrams                 uint64
	InboundFragmentBytes                     uint64
	InboundFragmentCompletedPackets          uint64
	InboundMalformedDatagrams                uint64
	InboundInvalidIPv4Packets                uint64
	InboundStackPackets                      uint64
	InboundStackBytes                        uint64
	InboundTCPPackets                        uint64
	InboundUDPPackets                        uint64
	InboundICMPPackets                       uint64
	InboundOtherPackets                      uint64
	InboundICMPDestinationUnreachablePackets uint64
	InboundICMPPortUnreachablePackets        uint64
}

type dataPathCounters struct {
	outboundStackPackets                     atomic.Uint64
	outboundStackBytes                       atomic.Uint64
	outboundUDPPackets                       atomic.Uint64
	outboundNodeDatagrams                    atomic.Uint64
	outboundNodeBytes                        atomic.Uint64
	inboundNodeDatagrams                     atomic.Uint64
	inboundNodeBytes                         atomic.Uint64
	inboundDataDatagrams                     atomic.Uint64
	inboundDataBytes                         atomic.Uint64
	inboundFragmentDatagrams                 atomic.Uint64
	inboundFragmentBytes                     atomic.Uint64
	inboundFragmentCompletedPackets          atomic.Uint64
	inboundMalformedDatagrams                atomic.Uint64
	inboundInvalidIPv4Packets                atomic.Uint64
	inboundStackPackets                      atomic.Uint64
	inboundStackBytes                        atomic.Uint64
	inboundTCPPackets                        atomic.Uint64
	inboundUDPPackets                        atomic.Uint64
	inboundICMPPackets                       atomic.Uint64
	inboundOtherPackets                      atomic.Uint64
	inboundICMPDestinationUnreachablePackets atomic.Uint64
	inboundICMPPortUnreachablePackets        atomic.Uint64
}

func (counters *dataPathCounters) recordOutboundStack(packet []byte) {
	counters.outboundStackPackets.Add(1)
	counters.outboundStackBytes.Add(uint64(len(packet)))
	if isIPv4UDP(packet) {
		counters.outboundUDPPackets.Add(1)
	}
}

func (counters *dataPathCounters) recordOutboundNode(bytes int) {
	counters.outboundNodeDatagrams.Add(1)
	counters.outboundNodeBytes.Add(uint64(bytes))
}

func (counters *dataPathCounters) recordInboundNode(packet []byte) {
	counters.inboundNodeDatagrams.Add(1)
	counters.inboundNodeBytes.Add(uint64(len(packet)))
}

func (counters *dataPathCounters) recordInboundData(packet []byte) {
	counters.recordInboundNode(packet)
	counters.inboundDataDatagrams.Add(1)
	counters.inboundDataBytes.Add(uint64(len(packet)))
}

func (counters *dataPathCounters) recordInboundFragment(packet []byte) {
	counters.recordInboundNode(packet)
	counters.inboundFragmentDatagrams.Add(1)
	counters.inboundFragmentBytes.Add(uint64(len(packet)))
}

func (counters *dataPathCounters) recordInboundStack(packet []byte) {
	counters.inboundStackPackets.Add(1)
	counters.inboundStackBytes.Add(uint64(len(packet)))
	headerLength, protocol, ok := ipv4Protocol(packet)
	if !ok {
		counters.inboundOtherPackets.Add(1)
		return
	}
	switch protocol {
	case ipv4ProtocolTCP:
		counters.inboundTCPPackets.Add(1)
	case ipv4ProtocolUDP:
		counters.inboundUDPPackets.Add(1)
	case ipv4ProtocolICMP:
		counters.inboundICMPPackets.Add(1)
		// Only the first IPv4 fragment contains the ICMP header.
		if binary.BigEndian.Uint16(packet[6:8])&0x1fff != 0 {
			return
		}
		if len(packet) >= headerLength+2 && packet[headerLength] == icmpv4TypeDestinationUnreachable {
			counters.inboundICMPDestinationUnreachablePackets.Add(1)
			if packet[headerLength+1] == icmpv4CodePortUnreachable {
				counters.inboundICMPPortUnreachablePackets.Add(1)
			}
		}
	default:
		counters.inboundOtherPackets.Add(1)
	}
}

func (counters *dataPathCounters) snapshot() DataPathStats {
	return DataPathStats{
		OutboundStackPackets:                     counters.outboundStackPackets.Load(),
		OutboundStackBytes:                       counters.outboundStackBytes.Load(),
		OutboundUDPPackets:                       counters.outboundUDPPackets.Load(),
		OutboundNodeDatagrams:                    counters.outboundNodeDatagrams.Load(),
		OutboundNodeBytes:                        counters.outboundNodeBytes.Load(),
		InboundNodeDatagrams:                     counters.inboundNodeDatagrams.Load(),
		InboundNodeBytes:                         counters.inboundNodeBytes.Load(),
		InboundDataDatagrams:                     counters.inboundDataDatagrams.Load(),
		InboundDataBytes:                         counters.inboundDataBytes.Load(),
		InboundFragmentDatagrams:                 counters.inboundFragmentDatagrams.Load(),
		InboundFragmentBytes:                     counters.inboundFragmentBytes.Load(),
		InboundFragmentCompletedPackets:          counters.inboundFragmentCompletedPackets.Load(),
		InboundMalformedDatagrams:                counters.inboundMalformedDatagrams.Load(),
		InboundInvalidIPv4Packets:                counters.inboundInvalidIPv4Packets.Load(),
		InboundStackPackets:                      counters.inboundStackPackets.Load(),
		InboundStackBytes:                        counters.inboundStackBytes.Load(),
		InboundTCPPackets:                        counters.inboundTCPPackets.Load(),
		InboundUDPPackets:                        counters.inboundUDPPackets.Load(),
		InboundICMPPackets:                       counters.inboundICMPPackets.Load(),
		InboundOtherPackets:                      counters.inboundOtherPackets.Load(),
		InboundICMPDestinationUnreachablePackets: counters.inboundICMPDestinationUnreachablePackets.Load(),
		InboundICMPPortUnreachablePackets:        counters.inboundICMPPortUnreachablePackets.Load(),
	}
}

func isIPv4UDP(packet []byte) bool {
	_, protocol, ok := ipv4Protocol(packet)
	return ok && protocol == ipv4ProtocolUDP
}

func ipv4Protocol(packet []byte) (int, byte, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return 0, 0, false
	}
	headerLength := int(packet[0]&0x0f) * 4
	if headerLength < 20 || headerLength > len(packet) {
		return 0, 0, false
	}
	return headerLength, packet[9], true
}
