//go:build game_proxy

package iwan

import (
	"encoding/binary"
	"testing"
)

func TestDataPathCounters_classifyInboundProtocols(t *testing.T) {
	var counters dataPathCounters
	counters.recordInboundStack(diagnosticIPv4(ipv4ProtocolTCP))
	counters.recordInboundStack(diagnosticIPv4(ipv4ProtocolUDP))
	counters.recordInboundStack(diagnosticIPv4(ipv4ProtocolICMP, icmpv4TypeDestinationUnreachable, 1))
	counters.recordInboundStack(diagnosticIPv4(ipv4ProtocolICMP, icmpv4TypeDestinationUnreachable, icmpv4CodePortUnreachable))
	counters.recordInboundStack(diagnosticIPv4(47))

	stats := counters.snapshot()
	if stats.InboundStackPackets != 5 || stats.InboundTCPPackets != 1 || stats.InboundUDPPackets != 1 ||
		stats.InboundICMPPackets != 2 || stats.InboundOtherPackets != 1 ||
		stats.InboundICMPDestinationUnreachablePackets != 2 || stats.InboundICMPPortUnreachablePackets != 1 {
		t.Fatalf("inbound protocol stats = %#v", stats)
	}
}

func TestDataPathCounters_classifyInboundNodeDatagrams(t *testing.T) {
	var counters dataPathCounters
	counters.recordInboundData(make([]byte, 44))
	counters.recordInboundFragment(make([]byte, 60))
	counters.inboundFragmentCompletedPackets.Add(1)
	counters.inboundMalformedDatagrams.Add(2)
	counters.inboundInvalidIPv4Packets.Add(3)

	stats := counters.snapshot()
	if stats.InboundNodeDatagrams != 2 || stats.InboundNodeBytes != 104 ||
		stats.InboundDataDatagrams != 1 || stats.InboundDataBytes != 44 ||
		stats.InboundFragmentDatagrams != 1 || stats.InboundFragmentBytes != 60 ||
		stats.InboundFragmentCompletedPackets != 1 || stats.InboundMalformedDatagrams != 2 ||
		stats.InboundInvalidIPv4Packets != 3 {
		t.Fatalf("inbound node stats = %#v", stats)
	}
}

func TestDataPathCounters_classifyICMPOnlyInFirstIPv4Fragment(t *testing.T) {
	var counters dataPathCounters
	for _, flagsAndOffset := range []uint16{0, 0x2000, 1, 0x100, 0x2001} {
		packet := diagnosticIPv4(ipv4ProtocolICMP, icmpv4TypeDestinationUnreachable, icmpv4CodePortUnreachable)
		binary.BigEndian.PutUint16(packet[6:8], flagsAndOffset)
		counters.recordInboundStack(packet)
	}
	stats := counters.snapshot()
	if stats.InboundICMPPackets != 5 || stats.InboundICMPDestinationUnreachablePackets != 2 ||
		stats.InboundICMPPortUnreachablePackets != 2 {
		t.Fatalf("fragment ICMP stats = %#v", stats)
	}
}

func diagnosticIPv4(protocol byte, transportHeader ...byte) []byte {
	packet := make([]byte, 20+len(transportHeader))
	packet[0] = 0x45
	packet[9] = protocol
	copy(packet[20:], transportHeader)
	return packet
}
