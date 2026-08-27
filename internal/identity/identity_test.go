package identity

import (
	"encoding/json"
	"testing"
)

func TestPeerIDTextAndJSONRoundTrip(t *testing.T) {
	peerID := PeerID{1}
	text := peerID.String()
	parsed, err := ParsePeerID(text)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != peerID {
		t.Fatal("peer ID text did not preserve the public key")
	}

	encoded, err := json.Marshal(peerID)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PeerID
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != peerID {
		t.Fatal("peer ID JSON did not preserve the public key")
	}
}
