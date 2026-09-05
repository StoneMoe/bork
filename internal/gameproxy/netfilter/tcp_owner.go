package netfilter

import (
	"encoding/binary"
	"net/netip"

	"bork/internal/gameproxy/intercept"
)

func tcpOwnerFromTable(table []byte, local, remote netip.AddrPort) (intercept.ProcessID, error) {
	// MIB_TCPTABLE_OWNER_PID is a DWORD count followed by six-DWORD rows.
	// Addresses and the low WORD of each port are in network byte order.
	const headerSize, rowSize = 4, 24
	if !validIPv4Endpoint(local) || !validIPv4Endpoint(remote) || len(table) < headerSize {
		return 0, ErrTCPRedirectOwner
	}
	count := binary.LittleEndian.Uint32(table[:headerSize])
	if uint64(count) > uint64((len(table)-headerSize)/rowSize) {
		return 0, ErrTCPRedirectOwner
	}
	var owner intercept.ProcessID
	for index := 0; index < int(count); index++ {
		row := table[headerSize+index*rowSize : headerSize+(index+1)*rowSize]
		rowLocal := netip.AddrPortFrom(netip.AddrFrom4([4]byte(row[4:8])), binary.BigEndian.Uint16(row[8:10]))
		rowRemote := netip.AddrPortFrom(netip.AddrFrom4([4]byte(row[12:16])), binary.BigEndian.Uint16(row[16:18]))
		if rowLocal != local || rowRemote != remote {
			continue
		}
		state := binary.LittleEndian.Uint32(row[:4])
		pid := intercept.ProcessID(binary.LittleEndian.Uint32(row[20:24]))
		// ESTABLISHED through LAST_ACK retain a live connection. Never trust
		// TIME_WAIT, a missing owner, or an ambiguous tuple in this snapshot.
		if state < 5 || state > 10 || pid == 0 || owner != 0 {
			return 0, ErrTCPRedirectOwner
		}
		owner = pid
	}
	if owner == 0 {
		return 0, ErrTCPRedirectOwner
	}
	return owner, nil
}
