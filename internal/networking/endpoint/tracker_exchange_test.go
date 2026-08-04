package endpoint

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"bork/internal/networking/discovery"
	trackerclient "bork/internal/networking/discovery/tracker"
)

func TestTrackerExchangeUsesEndpointSocketAndBypassesPeerQueues(t *testing.T) {
	tracker := listenTrackerTestUDP(t)
	endpointUDP, local, stop := runTrackerTestEndpoint(t)
	defer stop()

	transaction := uint32(0x10203040)
	request := trackerTestRequest(1, transaction)
	seenSource := make(chan netip.AddrPort, 1)
	go func() {
		buffer := make([]byte, 256)
		count, source, err := tracker.ReadFromUDPAddrPort(buffer)
		if err != nil {
			return
		}
		seenSource <- source
		response := trackerTestResponse(1, binary.BigEndian.Uint32(buffer[count-4:count]), []byte("peers"))
		_, _ = tracker.WriteToUDPAddrPort(response, source)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := endpointUDP.ExchangeTracker(ctx, tracker.LocalAddr().(*net.UDPAddr).AddrPort(), request, 1, transaction)
	if err != nil {
		t.Fatal(err)
	}
	if string(response[8:]) != "peers" {
		t.Fatalf("tracker response = %x", response)
	}
	select {
	case source := <-seenSource:
		if source.Port() != local.Port() {
			t.Fatalf("tracker source port = %d, endpoint port = %d", source.Port(), local.Port())
		}
	case <-time.After(time.Second):
		t.Fatal("tracker did not receive endpoint request")
	}
	select {
	case packet := <-endpointUDP.ControlPackets():
		t.Fatalf("tracker response reached control queue: %x", packet.Data)
	default:
	}
}

func TestTrackerExchangeIgnoresSpoofedAndMismatchedResponses(t *testing.T) {
	tracker := listenTrackerTestUDP(t)
	spoof := listenTrackerTestUDP(t)
	endpointUDP, _, stop := runTrackerTestEndpoint(t)
	defer stop()

	transaction := uint32(0x11223344)
	go func() {
		buffer := make([]byte, 256)
		_, source, err := tracker.ReadFromUDPAddrPort(buffer)
		if err != nil {
			return
		}
		_, _ = spoof.WriteToUDPAddrPort(trackerTestResponse(1, transaction, []byte("spoof")), source)
		_, _ = tracker.WriteToUDPAddrPort(trackerTestResponse(2, transaction, []byte("action")), source)
		_, _ = tracker.WriteToUDPAddrPort(trackerTestResponse(1, transaction+1, []byte("transaction")), source)
		time.Sleep(20 * time.Millisecond)
		_, _ = tracker.WriteToUDPAddrPort(trackerTestResponse(1, transaction, []byte("accepted")), source)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := endpointUDP.ExchangeTracker(ctx, tracker.LocalAddr().(*net.UDPAddr).AddrPort(), trackerTestRequest(1, transaction), 1, transaction)
	if err != nil {
		t.Fatal(err)
	}
	if string(response[8:]) != "accepted" {
		t.Fatalf("mismatched tracker response was accepted: %q", response[8:])
	}
}

func TestTrackerExchangeDeadlineAndPendingBound(t *testing.T) {
	tracker := listenTrackerTestUDP(t)
	endpointUDP, _, stop := runTrackerTestEndpoint(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := endpointUDP.ExchangeTracker(ctx, tracker.LocalAddr().(*net.UDPAddr).AddrPort(), trackerTestRequest(1, 1), 1, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline exchange error = %v", err)
	}

	endpointUDP.mu.Lock()
	for index := uint32(1); index <= maxPendingTracker; index++ {
		endpointUDP.trackers[index] = pendingTracker{result: make(chan []byte, 1)}
	}
	endpointUDP.mu.Unlock()
	boundedCtx, boundedCancel := context.WithTimeout(context.Background(), time.Second)
	defer boundedCancel()
	_, err = endpointUDP.ExchangeTracker(boundedCtx, tracker.LocalAddr().(*net.UDPAddr).AddrPort(), trackerTestRequest(1, maxPendingTracker+1), 1, maxPendingTracker+1)
	if err == nil || !strings.Contains(err.Error(), "too many pending") {
		t.Fatalf("pending bound error = %v", err)
	}
}

func TestTrackerExchangeUnblocksWhenEndpointStops(t *testing.T) {
	tracker := listenTrackerTestUDP(t)
	endpointUDP, _, cancelEndpoint, endpointResult := startTrackerTestEndpoint(t)
	transaction := uint32(0x55667788)
	exchangeResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := endpointUDP.ExchangeTracker(ctx, tracker.LocalAddr().(*net.UDPAddr).AddrPort(), trackerTestRequest(1, transaction), 1, transaction)
		exchangeResult <- err
	}()
	waitForPendingTracker(t, endpointUDP, transaction)
	cancelEndpoint()
	if err := <-endpointResult; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exchangeResult:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("shutdown exchange error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tracker exchange remained blocked after endpoint shutdown")
	}
}

func TestUDPTrackerAnnouncerUsesSharedEndpointEndToEnd(t *testing.T) {
	trackerServer := listenTrackerTestUDP(t)
	endpointUDP, local, stopEndpoint := runTrackerTestEndpoint(t)
	defer stopEndpoint()
	seenSources := make(chan netip.AddrPort, 2)
	go func() {
		buffer := make([]byte, 512)
		count, source, err := trackerServer.ReadFromUDPAddrPort(buffer)
		if err != nil || count != 16 {
			return
		}
		seenSources <- source
		transaction := binary.BigEndian.Uint32(buffer[12:16])
		connectResponse := make([]byte, 16)
		binary.BigEndian.PutUint32(connectResponse[0:4], 0)
		binary.BigEndian.PutUint32(connectResponse[4:8], transaction)
		binary.BigEndian.PutUint64(connectResponse[8:16], 0x0102030405060708)
		_, _ = trackerServer.WriteToUDPAddrPort(connectResponse, source)

		count, source, err = trackerServer.ReadFromUDPAddrPort(buffer)
		if err != nil || count != 98 {
			return
		}
		seenSources <- source
		transaction = binary.BigEndian.Uint32(buffer[12:16])
		announceResponse := make([]byte, 26)
		binary.BigEndian.PutUint32(announceResponse[0:4], 1)
		binary.BigEndian.PutUint32(announceResponse[4:8], transaction)
		binary.BigEndian.PutUint32(announceResponse[8:12], 30)
		copy(announceResponse[20:24], net.ParseIP("192.0.2.44").To4())
		binary.BigEndian.PutUint16(announceResponse[24:26], 9044)
		_, _ = trackerServer.WriteToUDPAddrPort(announceResponse, source)
	}()

	provider := "udp://" + trackerServer.LocalAddr().String() + "/announce"
	announcer, err := trackerclient.New([]string{provider}, [20]byte{1}, [32]byte{1}, endpointUDP, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	announcer.UpdateCandidates([]trackerclient.AnnounceCandidate{{Address: netip.MustParseAddr("192.0.2.99"), Port: local.Port()}})
	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	hints := make(chan discovery.Hint, 1)
	go func() { runResult <- announcer.Run(ctx, hints) }()
	select {
	case hint := <-hints:
		if hint.Source != discovery.SourceTracker || hint.Address != netip.MustParseAddrPort("192.0.2.44:9044") || hint.ExpiresAt.IsZero() {
			t.Fatalf("tracker hint = %#v", hint)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for UDP tracker hint")
	}
	for range 2 {
		select {
		case source := <-seenSources:
			if source.Port() != local.Port() {
				t.Fatalf("tracker request source = %s, endpoint = %s", source, local)
			}
		case <-time.After(time.Second):
			t.Fatal("tracker request was not observed")
		}
	}
	cancel()
	if err := <-runResult; err != nil {
		t.Fatal(err)
	}
}

func trackerTestRequest(action, transaction uint32) []byte {
	request := make([]byte, 16)
	binary.BigEndian.PutUint32(request[8:12], action)
	binary.BigEndian.PutUint32(request[12:16], transaction)
	return request
}

func trackerTestResponse(action, transaction uint32, body []byte) []byte {
	response := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(response[0:4], action)
	binary.BigEndian.PutUint32(response[4:8], transaction)
	copy(response[8:], body)
	return response
}

func listenTrackerTestUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func runTrackerTestEndpoint(t *testing.T) (*Endpoint, netip.AddrPort, func()) {
	t.Helper()
	endpointUDP, local, cancel, result := startTrackerTestEndpoint(t)
	return endpointUDP, local, func() {
		cancel()
		if err := <-result; err != nil {
			t.Errorf("Endpoint.Run() error = %v", err)
		}
	}
}

func startTrackerTestEndpoint(t *testing.T) (*Endpoint, netip.AddrPort, context.CancelFunc, <-chan error) {
	t.Helper()
	endpointUDP := New(Options{ListenAddress: "127.0.0.1:0", STUNServers: []string{}, STUNRefresh: 0}, endpointTestRoomTag, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- endpointUDP.Run(ctx) }()
	select {
	case <-endpointUDP.SnapshotChanges():
	case <-time.After(time.Second):
		cancel()
		t.Fatal("endpoint did not start")
	}
	local, err := netip.ParseAddrPort(endpointUDP.Snapshot().ListenAddress)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return endpointUDP, local, cancel, result
}

func waitForPendingTracker(t *testing.T, endpointUDP *Endpoint, transaction uint32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		endpointUDP.mu.RLock()
		_, exists := endpointUDP.trackers[transaction]
		endpointUDP.mu.RUnlock()
		if exists {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("tracker transaction was not registered")
}
