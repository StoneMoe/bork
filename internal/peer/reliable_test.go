package peer

import (
	"slices"
	"testing"
	"time"

	"bork/internal/protocol"
)

func TestReliableTransportDeliveryModes(t *testing.T) {
	tests := []struct {
		name    string
		channel uint16
		want    []string
	}{
		{name: "ordered", channel: reliableChannelTopology, want: []string{"hello world", "later"}},
		{name: "unordered", channel: reliableChannelFileData, want: []string{"later", "hello world"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			receiver := newReliableTransport()
			packets := []protocol.ReliablePacket{
				{Channel: test.channel, FragmentSequence: 3, FragmentCount: 1, Payload: []byte("later")},
				{Channel: test.channel, FragmentSequence: 2, FragmentIndex: 1, FragmentCount: 2, Payload: []byte(" world")},
				{Channel: test.channel, FragmentSequence: 1, FragmentCount: 2, Payload: []byte("hello")},
			}

			var got []string
			for _, packet := range packets {
				for _, message := range receiver.receive(packet, time.Time{}) {
					got = append(got, string(message.payload))
				}
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("delivered %q, want %q", got, test.want)
			}
		})
	}
}
