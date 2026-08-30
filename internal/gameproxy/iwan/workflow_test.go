package iwan

import (
	"testing"
	"time"
)

func TestWireWorkflow_authenticatesAndExchangesPackets(t *testing.T) {
	credentials, err := NewCredentials("myuser", "mypassword")
	if err != nil {
		t.Fatal(err)
	}
	openPacket, err := BuildOpen(credentials, DefaultMTU)
	if err != nil {
		t.Fatal(err)
	}
	request, err := ParseOpen(openPacket)
	if err != nil {
		t.Fatal(err)
	}
	if !request.Authenticate(credentials) {
		t.Fatal("OPEN authentication did not match configured credentials")
	}

	ackPacket := mustDecodeHex(t, "12011234deadbeef623721978dc2931a7569a4c8c6f5b0360304057804060a141e28080301")
	session, err := ParseOpenACK(ackPacket, credentials)
	if err != nil {
		t.Fatal(err)
	}
	dataPacket, err := session.BuildData([]byte("IPv4 packet"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := session.ParseData(dataPacket)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "IPv4 packet" {
		t.Fatalf("DATA payload = %q", payload)
	}

	echoRequest := BuildEchoRequest(session, Echo{Timestamp: time.Unix(10, 0)})
	echoResponse, err := BuildEchoResponse(session, echoRequest)
	if err != nil {
		t.Fatal(err)
	}
	control, err := ParseControl(echoResponse, session)
	if err != nil {
		t.Fatal(err)
	}
	if control.Type != TypeEchoResponse {
		t.Fatalf("control type = %#x", control.Type)
	}
}
