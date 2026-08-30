package iwan

import (
	"testing"
	"time"
)

func FuzzParseOpen(f *testing.F) {
	f.Add(mustDecodeHex(f, "13010000000000003601680de3a6b3dd336ac557c87b501b0304057801086d797573657202122712fab82fc2a7d17c8974e03e25be80080301"))
	f.Fuzz(func(t *testing.T, packet []byte) {
		_, _ = ParseOpen(packet)
	})
}

func FuzzParseOpenACK(f *testing.F) {
	credentials, err := NewCredentials("myuser", "mypassword")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(mustDecodeHex(f, "12011234deadbeef623721978dc2931a7569a4c8c6f5b0360304057804060a141e28080301"))
	f.Fuzz(func(t *testing.T, packet []byte) {
		_, _ = ParseOpenACK(packet, credentials)
	})
}

func FuzzReassemblerPush(f *testing.F) {
	session := goldenSession(f)
	f.Add(fragmentPacket(session, 1, 0, true, []byte("x")))
	f.Fuzz(func(t *testing.T, packet []byte) {
		reassembler := NewReassembler()
		_, _, _ = reassembler.Push(packet, session, time.Unix(100, 0))
	})
}
