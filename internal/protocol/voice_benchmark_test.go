package protocol

import "testing"

func BenchmarkVoiceProtection30Bytes(b *testing.B) {
	pair := newTestSessionPair(b)
	payload := make([]byte, 30)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		packet, err := MarshalVoice(pair.roomTag, pair.firstMaterial.SessionID, uint64(index+1), 480, payload, pair.firstCiphers.VoiceSend)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := ParseVoice(packet, pair.roomTag, pair.secondMaterial.SessionID, pair.secondCiphers.VoiceRecv); err != nil {
			b.Fatal(err)
		}
	}
}
