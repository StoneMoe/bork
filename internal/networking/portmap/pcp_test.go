package portmap

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestPCPMapperCreatesAndDeletesMapping(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	deleted := make(chan struct{}, 1)
	serverErrors := make(chan error, 1)
	go func() {
		buffer := make([]byte, 512)
		for {
			count, source, err := server.ReadFromUDPAddrPort(buffer)
			if err != nil {
				return
			}
			request := append([]byte(nil), buffer[:count]...)
			if len(request) != pcpMapMessageSize {
				serverErrors <- io.ErrUnexpectedEOF
				return
			}
			lifetime := binary.BigEndian.Uint32(request[4:8])
			mappedUnspecified, _ := pcpAddress(netip.IPv4Unspecified())
			wantAddress := mappedUnspecified[:]
			if lifetime == 0 {
				wantAddress = make([]byte, 16)
			}
			if string(request[44:60]) != string(wantAddress) {
				serverErrors <- io.ErrUnexpectedEOF
				return
			}
			if lifetime == 0 && binary.BigEndian.Uint16(request[42:44]) != 0 {
				serverErrors <- io.ErrUnexpectedEOF
				return
			}
			response := pcpTestResponse(request, lifetime, 100, netip.MustParseAddr("8.8.8.8"), 40000, 0)
			if lifetime == 0 {
				deleted <- struct{}{}
			}
			if _, err := server.WriteToUDPAddrPort(response, source); err != nil {
				serverErrors <- err
				return
			}
		}
	}()

	mapper := NewPCP(slog.New(slog.NewTextHandler(io.Discard, nil)))
	mapper.discover = func(context.Context) (pcpRoute, error) {
		return pcpRoute{gateway: netip.MustParseAddr("127.0.0.1"), local: netip.MustParseAddr("127.0.0.1")}, nil
	}
	mapper.gatewayPort = server.LocalAddr().(*net.UDPAddr).AddrPort().Port()
	mapper.timing.leaseDuration = time.Hour
	mapper.random = bytes.NewReader(bytes.Repeat([]byte{0x42}, 12))
	ctx, cancel := context.WithCancel(context.Background())
	states := make(chan State, 4)
	result := make(chan error, 1)
	go func() { result <- mapper.Run(ctx, 30000, states) }()
	select {
	case state := <-states:
		if state.Mapping == nil || state.Mapping.Provider != "PCP" || state.Mapping.ExternalAddress != netip.MustParseAddrPort("8.8.8.8:40000") {
			t.Fatalf("PCP state = %+v", state)
		}
	case err := <-serverErrors:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PCP mapping")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PCP mapper did not stop")
	}
	select {
	case <-deleted:
	case err := <-serverErrors:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("PCP mapper did not delete mapping")
	}
}

func TestPCPDoesNotPublishMappingThatExpiresDuringOperation(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	deleted := make(chan struct{}, 1)
	serverErrors := make(chan error, 1)
	go func() {
		buffer := make([]byte, 512)
		for {
			count, source, err := server.ReadFromUDPAddrPort(buffer)
			if err != nil {
				return
			}
			request := append([]byte(nil), buffer[:count]...)
			if len(request) != pcpMapMessageSize {
				serverErrors <- io.ErrUnexpectedEOF
				return
			}
			lifetime := uint32(1)
			if binary.BigEndian.Uint32(request[4:8]) == 0 {
				lifetime = 0
				deleted <- struct{}{}
			}
			response := pcpTestResponse(request, lifetime, 100, netip.MustParseAddr("8.8.8.8"), 40000, 0)
			if _, err := server.WriteToUDPAddrPort(response, source); err != nil {
				serverErrors <- err
				return
			}
		}
	}()

	mapper := NewPCP(slog.New(slog.NewTextHandler(io.Discard, nil)))
	mapper.discover = func(context.Context) (pcpRoute, error) {
		return pcpRoute{gateway: netip.MustParseAddr("127.0.0.1"), local: netip.MustParseAddr("127.0.0.1")}, nil
	}
	mapper.gatewayPort = server.LocalAddr().(*net.UDPAddr).AddrPort().Port()
	mapper.random = bytes.NewReader(bytes.Repeat([]byte{0x42}, 12))
	started := time.Unix(1000, 0)
	nowCalls := 0
	mapper.now = func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return started
		}
		return started.Add(2 * time.Second)
	}
	mapper.wait = func(context.Context, time.Duration) bool { return false }
	states := make(chan State, 1)
	if err := mapper.Run(context.Background(), 30000, states); err != nil {
		t.Fatal(err)
	}
	state := <-states
	if state.Mapping != nil || !strings.Contains(state.Error, "expired before it could be published") {
		t.Fatalf("PCP state = %+v", state)
	}
	select {
	case <-deleted:
	case err := <-serverErrors:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("PCP mapper did not clean up expired mapping")
	}
}

func TestPCPMapCodecAcceptsValidOptions(t *testing.T) {
	nonce := [12]byte{1, 2, 3}
	request, err := buildPCPMapRequest(netip.MustParseAddr("192.168.1.2"), nonce, 30000, 0, netip.Addr{}, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if request[0] != pcpVersion || request[1] != pcpMapOpcode || request[36] != pcpUDPProtocol {
		t.Fatalf("PCP request header = %x", request[:44])
	}
	response := pcpTestResponse(request, 1800, 20, netip.MustParseAddr("1.1.1.1"), 41000, 0)
	response = append(response, 1, 0, 0, 1, 0xaa, 0, 0, 0)
	parsed, err := parsePCPMapResponse(response, nonce, 30000)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.externalAddress != netip.MustParseAddrPort("1.1.1.1:41000") || parsed.lifetime != 1800 || parsed.epoch != 20 {
		t.Fatalf("PCP response = %+v", parsed)
	}
}

func TestPCPMapCodecRejectsMismatchesAndMalformedOptions(t *testing.T) {
	nonce := [12]byte{1}
	request, err := buildPCPMapRequest(netip.MustParseAddr("192.168.1.2"), nonce, 30000, 0, netip.Addr{}, 3600)
	if err != nil {
		t.Fatal(err)
	}
	response := pcpTestResponse(request, 1800, 20, netip.MustParseAddr("8.8.8.8"), 41000, 0)
	wrongNonce := nonce
	wrongNonce[0]++
	if _, err := parsePCPMapResponse(response, wrongNonce, 30000); err == nil {
		t.Fatal("accepted mismatched PCP nonce")
	}
	malformed := append(response, 1, 0, 0, 4, 1)
	if _, err := parsePCPMapResponse(malformed, nonce, 30000); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("malformed PCP option error = %v", err)
	}
	errorResponse := pcpTestResponse(request, 300, 20, netip.Addr{}, 0, 8)
	if _, err := parsePCPMapResponse(errorResponse, nonce, 30000); err == nil || !strings.Contains(err.Error(), "no resources") {
		t.Fatalf("PCP result error = %v", err)
	} else {
		var resultErr *pcpResultError
		if !errors.As(err, &resultErr) || resultErr.lifetime != 300 {
			t.Fatalf("PCP holdoff error = %#v", err)
		}
	}
}

func TestPCPRecognizesNATPMPUnsupportedVersion(t *testing.T) {
	for _, opcode := range []byte{0, 0x80} {
		response := make([]byte, 12)
		response[1] = opcode
		binary.BigEndian.PutUint16(response[2:4], 1)
		if !natPMPUnsupportedVersion(response) {
			t.Fatalf("NAT-PMP opcode %#x was not recognized", opcode)
		}
		if _, err := parsePCPMapResponse(response, [12]byte{}, 30000); err == nil || !strings.Contains(err.Error(), "does not support") {
			t.Fatalf("unsupported version error = %v", err)
		}
	}
}

func TestPCPEpochResetKeepsSuccessfulRenewal(t *testing.T) {
	owned := &pcpOwnedMapping{
		leaseSeconds: 3600,
		epoch:        100,
		mapping: Mapping{
			ExternalAddress: netip.MustParseAddrPort("8.8.8.8:40000"),
			Provider:        providerPCP,
			ExpiresAt:       time.Unix(1000, 0),
		},
	}
	started := time.Unix(2000, 0)
	response := pcpMapResponse{externalAddress: netip.MustParseAddrPort("1.1.1.1:41000"), lifetime: 1800, epoch: 5}
	if err := applyPCPRenewal(owned, response, started); !errors.Is(err, errPCPEpochReset) {
		t.Fatalf("epoch reset error = %v", err)
	}
	if owned.mapping.ExternalAddress != response.externalAddress || owned.epoch != 5 || !owned.mapping.ExpiresAt.Equal(started.Add(1800*time.Second)) {
		t.Fatalf("successful reset renewal was discarded: %+v", owned)
	}
}

func TestPCPRetryAfterIsBoundedByMappingExpiry(t *testing.T) {
	owned := &pcpOwnedMapping{mapping: Mapping{ExpiresAt: testStart.Add(10 * time.Second)}}
	state := State{RetryAfter: testStart.Add(6 * time.Second)}
	if delay := pcpRetryDelay(time.Second, state, owned, testStart); delay != 6*time.Second {
		t.Fatalf("RetryAfter delay = %s, want 6s", delay)
	}
	state.RetryAfter = testStart.Add(time.Minute)
	if delay := pcpRetryDelay(time.Second, state, owned, testStart); delay != 10*time.Second {
		t.Fatalf("expiry-bounded delay = %s, want 10s", delay)
	}
	if delay := pcpRetryDelay(time.Second, state, nil, testStart); delay != time.Second {
		t.Fatalf("unmapped retry delay = %s, want 1s", delay)
	}
}

func pcpTestResponse(request []byte, lifetime, epoch uint32, externalAddress netip.Addr, externalPort uint16, result byte) []byte {
	response := make([]byte, pcpMapMessageSize)
	response[0] = pcpVersion
	response[1] = pcpResponseBit | pcpMapOpcode
	response[3] = result
	binary.BigEndian.PutUint32(response[4:8], lifetime)
	binary.BigEndian.PutUint32(response[8:12], epoch)
	copy(response[24:42], request[24:42])
	binary.BigEndian.PutUint16(response[42:44], externalPort)
	if externalAddress.IsValid() {
		encoded, _ := pcpAddress(externalAddress)
		copy(response[44:60], encoded[:])
	}
	return response
}
