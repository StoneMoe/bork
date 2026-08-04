package audio

import "testing"

func TestRecommendedVoiceBitrateScalesWithoutMemberCap(t *testing.T) {
	previous := defaultVoiceBitrate + 1
	for members := 1; members <= 10000; members++ {
		bitrate := RecommendedVoiceBitrate(members)
		if bitrate < minimumVoiceBitrate || bitrate > defaultVoiceBitrate {
			t.Fatalf("members %d: bitrate %d outside bounds", members, bitrate)
		}
		if bitrate > previous {
			t.Fatalf("members %d: bitrate increased from %d to %d", members, previous, bitrate)
		}
		previous = bitrate
	}
}

func TestOpusEncoderEntersDTXForSilence(t *testing.T) {
	encoder, err := newOpusEncoder(512)
	if err != nil {
		t.Fatal(err)
	}
	silence := make([]float32, FrameSamples)
	for range 200 {
		if _, err := encoder.Encode(silence); err != nil {
			t.Fatal(err)
		}
		if encoder.codec.InDTX() {
			return
		}
	}
	t.Fatal("encoder did not enter DTX after two seconds of silence")
}
