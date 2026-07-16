package discovery

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLocalDiscoverySeparatesRooms(t *testing.T) {
	first := testLocalDiscovery()
	second := testLocalDiscovery()
	firstRoom := testRoomTag(t)
	secondRoom := testRoomTag(t)
	found := make(chan netip.AddrPort, 2)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		firstDone <- first.Run(ctx, firstRoom, netip.MustParseAddrPort("127.0.0.1:41001"), found)
	}()
	go func() {
		secondDone <- second.Run(ctx, secondRoom, netip.MustParseAddrPort("127.0.0.1:41002"), found)
	}()
	select {
	case peer := <-found:
		t.Fatalf("discovered peer from another room: %#v", peer)
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
}

func TestLocalDiscoveryAcrossProcesses(t *testing.T) {
	roomTag := testRoomTag(t)
	var output bytes.Buffer
	command := exec.Command(os.Args[0], "-test.run=^TestLocalDiscoveryHelper$")
	command.Env = append(os.Environ(),
		"BORK_LOCAL_DISCOVERY_HELPER=1",
		"BORK_LOCAL_DISCOVERY_ROOM="+hex.EncodeToString(roomTag[:]),
	)
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	helperDone := false
	t.Cleanup(func() {
		if !helperDone {
			_ = command.Process.Kill()
			<-waitDone
		}
	})

	local := testLocalDiscovery()
	found := make(chan netip.AddrPort, 2)
	ctx, cancel := context.WithCancel(context.Background())
	localDone := make(chan error, 1)
	go func() {
		localDone <- local.Run(ctx, roomTag, netip.MustParseAddrPort("127.0.0.1:41001"), found)
	}()
	waitForPeer(t, found, netip.MustParseAddrPort("127.0.0.1:41002"))
	select {
	case err := <-waitDone:
		helperDone = true
		if err != nil {
			t.Fatalf("helper process error = %v\n%s", err, output.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("helper did not discover parent\n%s", output.String())
	}
	cancel()
	if err := <-localDone; err != nil {
		t.Fatalf("parent Run() error = %v", err)
	}
	if !strings.Contains(output.String(), "FOUND 127.0.0.1:41001") {
		t.Fatalf("helper output did not contain parent discovery: %s", output.String())
	}
}

func TestLocalDiscoveryHelper(t *testing.T) {
	if os.Getenv("BORK_LOCAL_DISCOVERY_HELPER") != "1" {
		return
	}
	encodedRoom, err := hex.DecodeString(os.Getenv("BORK_LOCAL_DISCOVERY_ROOM"))
	if err != nil || len(encodedRoom) != 16 {
		t.Fatalf("invalid helper RoomTag: %q", os.Getenv("BORK_LOCAL_DISCOVERY_ROOM"))
	}
	var roomTag [16]byte
	copy(roomTag[:], encodedRoom)
	local := testLocalDiscovery()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	found := make(chan netip.AddrPort, 2)
	done := make(chan error, 1)
	go func() {
		done <- local.Run(ctx, roomTag, netip.MustParseAddrPort("127.0.0.1:41002"), found)
	}()
	select {
	case peer := <-found:
		fmt.Printf("FOUND %s\n", peer)
		time.Sleep(100 * time.Millisecond)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("local discovery stopped before finding parent")
	case <-ctx.Done():
		t.Fatal("timed out waiting for parent")
	}
}

func TestLocalAnnouncementRoundTrip(t *testing.T) {
	want := localAnnouncement{
		roomTag:  [16]byte{4},
		peerHint: "peer-a",
		address:  netip.MustParseAddrPort("[::1]:41001"),
	}
	packet, err := marshalLocalAnnouncement(want.roomTag, want.peerHint, want.address)
	if err != nil {
		t.Fatalf("marshalLocalAnnouncement() error = %v", err)
	}
	got, err := parseLocalAnnouncement(packet)
	if err != nil {
		t.Fatalf("parseLocalAnnouncement() error = %v", err)
	}
	if got != want {
		t.Fatalf("announcement = %#v, want %#v", got, want)
	}
	tests := [][]byte{
		nil,
		packet[:len(localAnnouncementMagic)-1],
		append([]byte("NOTLOCAL"), packet[len(localAnnouncementMagic):]...),
		append(append([]byte(nil), packet...), 0),
	}
	invalidPeer := append([]byte(nil), packet...)
	invalidPeer[len(localAnnouncementMagic)+16+2] = 0
	tests = append(tests, invalidPeer)
	invalidAddress := append([]byte(nil), packet...)
	invalidAddress[len(invalidAddress)-1] ^= 0xff
	tests = append(tests, invalidAddress)
	for index, malformed := range tests {
		if _, err := parseLocalAnnouncement(malformed); err == nil {
			t.Fatalf("parseLocalAnnouncement() error = nil for malformed packet %d", index)
		}
	}
}

func TestReadLocalDatagramsDropsOversizedDatagramAndContinues(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	incoming := make(chan localDatagram, 2)
	done := make(chan error, 1)
	readerStopped := false
	go readLocalDatagrams(ctx, receiver, incoming, done)
	defer func() {
		cancel()
		_ = receiver.Close()
		if readerStopped {
			return
		}
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("readLocalDatagrams() error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("local datagram reader did not stop")
		}
	}()

	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("sender ListenUDP() error = %v", err)
	}
	defer sender.Close()
	destination := receiver.LocalAddr().(*net.UDPAddr)
	if _, err := sender.WriteToUDP(make([]byte, localAnnouncementSize()+2), destination); err != nil {
		t.Fatalf("write oversized datagram: %v", err)
	}
	want, err := marshalLocalAnnouncement([16]byte{1}, "peer-a", netip.MustParseAddrPort("127.0.0.1:41001"))
	if err != nil {
		t.Fatalf("marshalLocalAnnouncement() error = %v", err)
	}
	if _, err := sender.WriteToUDP(want, destination); err != nil {
		t.Fatalf("write valid datagram: %v", err)
	}

	select {
	case datagram := <-incoming:
		if !bytes.Equal(datagram.data, want) {
			t.Fatalf("received datagram length = %d, want valid datagram length %d", len(datagram.data), len(want))
		}
	case err := <-done:
		readerStopped = true
		t.Fatalf("local datagram reader stopped after oversized datagram: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for valid local datagram")
	}
}

func TestLocalAnnouncementRejectsNonLocalAddress(t *testing.T) {
	if _, err := marshalLocalAnnouncement([16]byte{1}, "peer-a", netip.MustParseAddrPort("192.0.2.1:41001")); err == nil {
		t.Fatal("marshalLocalAnnouncement() error = nil for non-local address")
	}
}

func TestLoopbackAddressUsesMatchingFamily(t *testing.T) {
	ipv4, err := loopbackAddress(netip.MustParseAddrPort("0.0.0.0:7000"))
	if err != nil || ipv4 != netip.MustParseAddrPort("127.0.0.1:7000") {
		t.Fatalf("IPv4 loopbackAddress() = %v, %v", ipv4, err)
	}
	ipv6, err := loopbackAddress(netip.MustParseAddrPort("[::]:7000"))
	if err != nil || ipv6 != netip.MustParseAddrPort("[::1]:7000") {
		t.Fatalf("IPv6 loopbackAddress() = %v, %v", ipv6, err)
	}
}

func testLocalDiscovery() *localDiscovery {
	return &localDiscovery{announceInterval: 20 * time.Millisecond}
}

func testRoomTag(t *testing.T) [16]byte {
	t.Helper()
	var roomTag [16]byte
	if _, err := rand.Read(roomTag[:]); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	return roomTag
}

func waitForPeer(t *testing.T, found <-chan netip.AddrPort, address netip.AddrPort) {
	t.Helper()
	select {
	case peer := <-found:
		if peer != address {
			t.Fatalf("found peer = %#v, want %s", peer, address)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for local peer at %s", address)
	}
}

func FuzzParseLocalAnnouncement(f *testing.F) {
	packet, err := marshalLocalAnnouncement([16]byte{1}, "peer-a", netip.MustParseAddrPort("127.0.0.1:41001"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(packet)
	f.Add([]byte("not-local-discovery"))
	f.Fuzz(func(t *testing.T, packet []byte) {
		_, _ = parseLocalAnnouncement(packet)
	})
}
