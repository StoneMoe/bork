package invite

import (
	"encoding/binary"
	"testing"
)

func TestInviteUsesCRC16Checksum(t *testing.T) {
	invite := fixedInvite()
	encoded := invite.Encode()
	payload, err := decodeBase58(encoded[len(prefix):])
	if err != nil {
		t.Fatal(err)
	}
	checksum := binary.BigEndian.Uint16(payload[len(payload)-checksumSize:])
	if payload[0] != Version || checksum != 0xc257 {
		t.Fatalf("version = %d, checksum = %04x", payload[0], checksum)
	}
	parsed, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != invite {
		t.Fatalf("parsed invite = %+v", parsed)
	}
	payload[1] ^= 1
	if _, err := Parse(prefix + encodeBase58(payload)); err == nil {
		t.Fatal("tampered invite passed CRC-16 validation")
	}
}

func TestInviteCRC16StandardVector(t *testing.T) {
	if checksum := inviteCRC16([]byte("123456789")); checksum != 0x29b1 {
		t.Fatalf("CRC-16/CCITT-FALSE checksum = %04x", checksum)
	}
}

func fixedInvite() Invite {
	invite := Invite{DisplayName: "test room"}
	for index := range invite.roomSeed {
		invite.roomSeed[index] = byte(index)
	}
	return invite
}
