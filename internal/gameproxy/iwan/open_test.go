package iwan

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func TestBuildOpen_matchesPinnedGoldenVector(t *testing.T) {
	credentials, err := NewCredentials(" myuser ", "mypassword")
	if err != nil {
		t.Fatal(err)
	}

	packet, err := BuildOpen(credentials, DefaultMTU)
	if err != nil {
		t.Fatal(err)
	}

	want := mustDecodeHex(t, "13010000000000003601680de3a6b3dd336ac557c87b501b0304057801086d797573657202122712fab82fc2a7d17c8974e03e25be80080301")
	if !bytes.Equal(packet, want) {
		t.Fatalf("BuildOpen() = %x, want %x", packet, want)
	}
}

func TestParseOpen_returnsTypedAuthenticationFields(t *testing.T) {
	packet := mustDecodeHex(t, "13010000000000003601680de3a6b3dd336ac557c87b501b0304057801086d797573657202122712fab82fc2a7d17c8974e03e25be80080301")

	request, err := ParseOpen(packet)
	if err != nil {
		t.Fatal(err)
	}

	if request.Username != "myuser" || request.MTU != 1400 || !request.XOR {
		t.Fatalf("ParseOpen() = %#v", request)
	}
	wantPassword := mustDecodeHex(t, "2712fab82fc2a7d17c8974e03e25be80")
	if !bytes.Equal(request.PasswordBlock[:], wantPassword) {
		t.Fatalf("password block = %x, want %x", request.PasswordBlock, wantPassword)
	}
}

func TestNewCredentials_matchesConfigByteBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		wantErr  bool
	}{
		{name: "maximum UTF-8 byte lengths", username: strings.Repeat("界", 84) + "a", password: strings.Repeat("界", 5) + "a"},
		{name: "username over 253 bytes", username: strings.Repeat("u", 254), password: "p", wantErr: true},
		{name: "password over 16 bytes", username: "u", password: strings.Repeat("p", 17), wantErr: true},
		{name: "blank normalized username", username: " \t ", password: "p", wantErr: true},
		{name: "empty password", username: "u", password: "", wantErr: true},
		{name: "invalid username UTF-8", username: string([]byte{0xff}), password: "p", wantErr: true},
		{name: "invalid password UTF-8", username: "u", password: string([]byte{0xff}), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewCredentials(test.username, test.password)
			if (err != nil) != test.wantErr {
				t.Fatalf("NewCredentials() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestParseOpen_rejectsMalformedTLVAndUnknownCriticalTag(t *testing.T) {
	valid := mustDecodeHex(t, "13010000000000003601680de3a6b3dd336ac557c87b501b0304057801086d797573657202122712fab82fc2a7d17c8974e03e25be80080301")
	tests := [][]byte{
		append(append([]byte(nil), valid...), 0x01),
		append(append([]byte(nil), valid...), 0x80, 0x02),
		append(append([]byte(nil), valid...), 0x01, 0xff),
	}
	for _, packet := range tests {
		if _, err := ParseOpen(packet); err == nil {
			t.Fatalf("ParseOpen(%x) succeeded", packet)
		}
	}
}

func mustDecodeHex(t testing.TB, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
