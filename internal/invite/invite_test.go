package invite

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestInviteUsesCompactBase58Payload(t *testing.T) {
	var roomSeed [RoomSeedSize]byte
	for index := range roomSeed {
		roomSeed[index] = byte(index + 1)
	}
	roomInvite := Invite{DisplayName: "room", roomSeed: roomSeed}
	encoded := strings.TrimPrefix(roomInvite.Encode(), prefix)
	for _, excluded := range "0OIl-_=" {
		if strings.ContainsRune(encoded, excluded) {
			t.Fatalf("Base58 invite contains %q: %s", excluded, encoded)
		}
	}
	payload, err := decodeBase58(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 1+RoomSeedSize+len(roomInvite.DisplayName)+checksumSize {
		t.Fatalf("compact payload length = %d", len(payload))
	}
	legacyPayloadSize := 1 + RoomSeedSize + 2 + len(roomInvite.DisplayName) + checksumSize
	if len(encoded) >= base64.RawURLEncoding.EncodedLen(legacyPayloadSize) {
		t.Fatalf("Base58 invite length %d did not improve on legacy Base64URL length %d", len(encoded), base64.RawURLEncoding.EncodedLen(legacyPayloadSize))
	}
}

func TestBase58PreservesLeadingZeroes(t *testing.T) {
	input := []byte{0, 0, 1, 2, 3, 255}
	encoded := encodeBase58(input)
	decoded, err := decodeBase58(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(input) || !strings.HasPrefix(encoded, "11") {
		t.Fatalf("Base58 round trip = %q, %v", encoded, decoded)
	}
}

func TestInviteRoundTripAndDerivation(t *testing.T) {
	created, err := New("  Night Shift  ")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	parsed, err := Parse(created.Encode())
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if created != parsed {
		t.Fatalf("round trip changed invite: %#v != %#v", created, parsed)
	}
	if parsed.DisplayName != "Night Shift" {
		t.Fatalf("parsed invite = %#v", parsed)
	}
	if created.TrackerHash() != parsed.TrackerHash() || created.AdmissionKey() != parsed.AdmissionKey() || created.GroupMediaKey() != parsed.GroupMediaKey() {
		t.Fatal("derived values changed after round trip")
	}
}

func TestNewInvitesHaveDifferentRooms(t *testing.T) {
	first, err := New("same name")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	second, err := New("same name")
	if err != nil {
		t.Fatalf("New() second error = %v", err)
	}
	if first.RoomTag() == second.RoomTag() {
		t.Fatal("independent invites produced the same room tag")
	}
}

func TestHKDFDerivationVector(t *testing.T) {
	var roomSeed [RoomSeedSize]byte
	for index := range roomSeed {
		roomSeed[index] = byte(index)
	}
	roomInvite := Invite{DisplayName: "vector", roomSeed: roomSeed}
	trackerHash := roomInvite.TrackerHash()
	admissionKey := roomInvite.AdmissionKey()
	roomTag := roomInvite.RoomTag()
	got := []string{
		hex.EncodeToString(trackerHash[:]),
		hex.EncodeToString(admissionKey[:]),
		hex.EncodeToString(roomTag[:]),
	}
	want := []string{
		"4dc346c3735e1b454935d58e9fafb7c59f1a8626",
		"7629c8f078da9a9a93a62e0459289c9f95f26997e15d9aae667bdf11763e8f93",
		"2c78d6ac8916fc16ed542313651af129",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("derivation vectors = %#v", got)
	}
}

func TestParseRejectsTamperedInvite(t *testing.T) {
	created, err := New("room")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	encoded := strings.TrimPrefix(created.Encode(), prefix)
	payload, err := decodeBase58(encoded)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	payload[5] ^= 0xff
	tampered := prefix + encodeBase58(payload)
	if _, err := Parse(tampered); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Parse() error = %v, want checksum error", err)
	}
}

func TestParseRejectsNonCanonicalRoomName(t *testing.T) {
	roomInvite := Invite{DisplayName: " room "}
	if _, err := Parse(roomInvite.Encode()); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("Parse() error = %v, want canonical name error", err)
	}
}

func TestParseRejectsInvalidInvite(t *testing.T) {
	tests := []string{"", "bork://join/", "not-base58-0OIl", "bork://join/2"}
	for _, encoded := range tests {
		if _, err := Parse(encoded); err == nil {
			t.Fatalf("Parse(%q) error = nil", encoded)
		}
	}
}

func TestParseRejectsOversizedWhitespace(t *testing.T) {
	if _, err := Parse(strings.Repeat(" ", MaxEncodedSize+1)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("Parse() error = %v, want size error", err)
	}
}

func TestParseRejectsEmbeddedNewline(t *testing.T) {
	created, err := New("room")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	encoded := created.Encode()
	encoded = encoded[:len(encoded)-2] + "\n" + encoded[len(encoded)-2:]
	if _, err := Parse(encoded); err == nil {
		t.Fatal("Parse() error = nil for embedded newline")
	}
}

func TestNewRejectsInvalidDisplayName(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New() error = nil for empty name")
	}
	if _, err := New(strings.Repeat("x", MaxDisplayRunes+1)); err == nil {
		t.Fatal("New() error = nil for long name")
	}
}

func TestMaximumUTF8RoomNameFitsInviteLimit(t *testing.T) {
	roomInvite, err := New(strings.Repeat("😀", MaxDisplayRunes))
	if err != nil {
		t.Fatal(err)
	}
	if encoded := roomInvite.Encode(); len(encoded) > MaxEncodedSize {
		t.Fatalf("maximum invite length = %d, limit %d", len(encoded), MaxEncodedSize)
	} else if parsed, err := Parse(encoded); err != nil || parsed != roomInvite {
		t.Fatalf("maximum invite did not round trip: %v", err)
	}
}

func FuzzParseInvite(f *testing.F) {
	roomInvite, err := New("fuzz seed")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(roomInvite.Encode())
	f.Add("not-an-invite")
	f.Fuzz(func(t *testing.T, encoded string) {
		parsed, err := Parse(encoded)
		if err != nil {
			return
		}
		roundTrip, err := Parse(parsed.Encode())
		if err != nil || parsed != roundTrip {
			t.Fatalf("valid invite did not round trip: %v", err)
		}
	})
}
