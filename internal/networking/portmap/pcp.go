package portmap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"time"
)

const (
	providerPCP        = "PCP"
	pcpVersion         = 2
	pcpMapOpcode       = 1
	pcpResponseBit     = 0x80
	pcpUDPProtocol     = 17
	pcpMapMessageSize  = 60
	pcpGatewayPort     = 5351
	pcpReadPoll        = 100 * time.Millisecond
	pcpRequestAttempts = 3
	pcpIPv6Anycast     = "2001:1::1"
)

var errPCPEpochReset = errors.New("PCP gateway epoch moved backwards")

type pcpRoute struct {
	gateway netip.Addr
	local   netip.Addr
}

type pcpDiscoverFunc func(context.Context) (pcpRoute, error)

type PCP struct {
	logger      *slog.Logger
	discover    pcpDiscoverFunc
	gatewayPort uint16
	random      io.Reader
	now         func() time.Time
	wait        waitFunc
	timing      mapperTiming
	running     atomic.Bool
}

type pcpOwnedMapping struct {
	nonce        [12]byte
	internalPort uint16
	leaseSeconds uint32
	epoch        uint32
	mapping      Mapping
}

type pcpMapResponse struct {
	externalAddress netip.AddrPort
	lifetime        uint32
	epoch           uint32
}

type pcpResultError struct {
	code     byte
	lifetime uint32
}

func (err *pcpResultError) Error() string {
	return fmt.Sprintf("PCP result %d (%s)", err.code, pcpResultText(err.code))
}

type pcpGatewayClient struct {
	conn *net.UDPConn
}

var _ Mapper = (*PCP)(nil)

func NewPCP(logger *slog.Logger) *PCP {
	return newPCP(logger, discoverPCPGateway)
}

func newPCP(logger *slog.Logger, discover pcpDiscoverFunc) *PCP {
	if logger == nil {
		logger = slog.Default()
	}
	return &PCP{
		logger:      logger,
		discover:    discover,
		gatewayPort: pcpGatewayPort,
		random:      rand.Reader,
		now:         time.Now,
		wait:        waitContext,
		timing:      defaultMapperTiming,
	}
}

func (p *PCP) Run(ctx context.Context, internalPort uint16, states chan<- State) error {
	if ctx == nil {
		return errors.New("PCP context is required")
	}
	if internalPort == 0 {
		return errors.New("PCP requires a non-zero internal port")
	}
	if states == nil {
		return errors.New("PCP state channel is required")
	}
	if !p.running.CompareAndSwap(false, true) {
		return errors.New("PCP mapper is already running")
	}
	defer p.running.Store(false)

	timing := normalizeTiming(p.timing)
	retry := timing.retryInitial
	var nonce [12]byte
	if _, err := io.ReadFull(p.random, nonce[:]); err != nil {
		return fmt.Errorf("create PCP nonce: %w", err)
	}
	var client *pcpGatewayClient
	var owned *pcpOwnedMapping
	attempted := false
	defer func() {
		if client == nil {
			return
		}
		if attempted {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), timing.cleanupTimeout)
			if err := deletePCPMapping(cleanupCtx, client, nonce, internalPort); err != nil {
				p.logger.Debug("PCP cleanup failed", "error", boundedError(err))
			}
			cancel()
		}
		_ = client.Close()
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}
		if owned != nil && !p.now().Before(owned.mapping.ExpiresAt) {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), timing.cleanupTimeout)
			_ = deletePCPMapping(cleanupCtx, client, nonce, internalPort)
			cancel()
			owned = nil
			attempted = false
			_ = client.Close()
			client = nil
			if !publishState(ctx, states, State{Error: "PCP mapping expired"}) {
				return nil
			}
		}

		var attemptErr error
		if client == nil {
			discoverCtx, cancel := context.WithTimeout(ctx, timing.discoveryTimeout)
			route, err := p.discover(discoverCtx)
			cancel()
			if err == nil {
				client, err = newPCPGatewayClient(route, p.gatewayPort)
			}
			if err != nil {
				attemptErr = fmt.Errorf("discover PCP gateway: %w", err)
			}
		}
		if attemptErr == nil && owned == nil {
			started := p.now()
			attempted = true
			response, err := requestPCPMapping(ctx, client, client.localAddress(), nonce, internalPort, internalPort, netip.Addr{}, leaseSeconds(timing.leaseDuration))
			if err != nil {
				attemptErr = fmt.Errorf("create PCP UDP mapping: %w", err)
			} else {
				owned, attemptErr = newPCPOwnedMapping(nonce, internalPort, leaseSeconds(timing.leaseDuration), response, started)
			}
		} else if attemptErr == nil {
			started := p.now()
			response, err := requestPCPMapping(ctx, client, client.localAddress(), owned.nonce, internalPort, owned.mapping.ExternalAddress.Port(), owned.mapping.ExternalAddress.Addr(), owned.leaseSeconds)
			if err != nil {
				attemptErr = fmt.Errorf("renew PCP UDP mapping: %w", err)
			} else {
				attemptErr = applyPCPRenewal(owned, response, started)
			}
		}

		if ctx.Err() != nil {
			return nil
		}
		if owned != nil && !mappingValidAt(owned.mapping, p.now()) {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), timing.cleanupTimeout)
			_ = deletePCPMapping(cleanupCtx, client, nonce, internalPort)
			cancel()
			owned = nil
			attempted = false
			_ = client.Close()
			client = nil
			if attemptErr == nil {
				attemptErr = errors.New("PCP mapping expired before it could be published")
			}
		}
		if attemptErr == nil {
			retry = timing.retryInitial
			if !publishState(ctx, states, State{Mapping: copyMapping(&owned.mapping)}) {
				return nil
			}
			if !p.wait(ctx, mappingRefreshDelay(owned.mapping, owned.leaseSeconds, p.now())) {
				return nil
			}
			continue
		}

		state := State{Error: boundedError(attemptErr)}
		var resultErr *pcpResultError
		if errors.As(attemptErr, &resultErr) {
			holdoff := min(time.Duration(resultErr.lifetime)*time.Second, time.Hour)
			if holdoff > 0 {
				state.RetryAfter = p.now().Add(holdoff)
			}
			if owned != nil && (resultErr.code == 2 || resultErr.code == 12) {
				owned = nil
			}
		}
		if owned != nil && p.now().Before(owned.mapping.ExpiresAt) {
			state.Mapping = copyMapping(&owned.mapping)
		}
		if !publishState(ctx, states, state) {
			return nil
		}
		p.logger.Warn("PCP port mapping unavailable", "error", state.Error)
		if owned == nil && client != nil {
			if attempted {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), timing.cleanupTimeout)
				_ = deletePCPMapping(cleanupCtx, client, nonce, internalPort)
				cancel()
				attempted = false
			}
			_ = client.Close()
			client = nil
		}
		delay := pcpRetryDelay(retry, state, owned, p.now())
		if !p.wait(ctx, delay) {
			return nil
		}
		retry = nextRetry(retry, timing.retryMaximum)
	}
}

func discoverPCPGateway(ctx context.Context) (pcpRoute, error) {
	route, err := discoverDefaultRoute(ctx)
	if err != nil {
		return pcpRoute{}, err
	}
	return pcpRouteFromDefault(route, false)
}

func discoverIPv6PCPGateway(ctx context.Context) (pcpRoute, error) {
	route, err := discoverDefaultIPv6Route(ctx)
	if err != nil {
		return pcpRoute{}, err
	}
	return pcpRouteFromDefault(route, true)
}

func discoverIPv6PCPAnycast(_ context.Context) (pcpRoute, error) {
	return pcpRoute{gateway: netip.MustParseAddr(pcpIPv6Anycast)}, nil
}

func pcpRouteFromDefault(route defaultRoute, ipv6 bool) (pcpRoute, error) {
	local, ok := netip.AddrFromSlice(route.local)
	if !ok || !usablePCPLocalAddress(local.Unmap(), ipv6) {
		return pcpRoute{}, errors.New("PCP local address is not usable")
	}
	gateway, ok := netip.AddrFromSlice(route.gateway)
	if !ok || !usablePCPGatewayAddress(gateway.Unmap(), ipv6) {
		return pcpRoute{}, errors.New("PCP gateway address is not usable")
	}
	local, gateway = local.Unmap(), gateway.Unmap()
	if gateway.IsLinkLocalUnicast() {
		if route.zone == "" {
			return pcpRoute{}, errors.New("PCP link-local gateway has no interface zone")
		}
		gateway = gateway.WithZone(route.zone)
	}
	return pcpRoute{gateway: gateway, local: local}, nil
}

func usablePCPLocalAddress(address netip.Addr, ipv6 bool) bool {
	return address.IsValid() &&
		address.Is6() == ipv6 &&
		address.IsGlobalUnicast() &&
		(!ipv6 || !address.IsPrivate())
}

func usablePCPGatewayAddress(address netip.Addr, ipv6 bool) bool {
	if !address.IsValid() || address.Is6() != ipv6 {
		return false
	}
	return address.IsGlobalUnicast() || (ipv6 && address.IsLinkLocalUnicast())
}

func newPCPGatewayClient(route pcpRoute, port uint16) (*pcpGatewayClient, error) {
	network, local, gateway := pcpDialAddresses(route, port)
	conn, err := net.DialUDP(network, local, gateway)
	if err != nil {
		return nil, err
	}
	actualLocal := conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	if !usablePCPLocalAddress(actualLocal, route.gateway.Is6()) {
		_ = conn.Close()
		return nil, errors.New("PCP socket selected an unusable local address")
	}
	return &pcpGatewayClient{conn: conn}, nil
}

// A nil local address lets the OS select the source for the RFC 7723 anycast
// fallback; the PCP header then uses conn.LocalAddr().
func pcpDialAddresses(route pcpRoute, port uint16) (string, *net.UDPAddr, *net.UDPAddr) {
	network := "udp4"
	if route.gateway.Is6() {
		network = "udp6"
	}
	gateway := net.UDPAddrFromAddrPort(netip.AddrPortFrom(route.gateway, port))
	if !route.local.IsValid() {
		return network, nil, gateway
	}
	local := net.UDPAddrFromAddrPort(netip.AddrPortFrom(route.local, 0))
	return network, local, gateway
}

func (c *pcpGatewayClient) Close() error { return c.conn.Close() }

func (c *pcpGatewayClient) localAddress() netip.Addr {
	return c.conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
}

func (c *pcpGatewayClient) exchange(ctx context.Context, request []byte, match func([]byte) bool) ([]byte, error) {
	buffer := make([]byte, 2048)
	for attempt := range pcpRequestAttempts {
		timeout := time.Second << attempt
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(timeout)
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		if err := c.conn.SetWriteDeadline(deadline); err != nil {
			return nil, err
		}
		if _, err := c.conn.Write(request); err != nil {
			if isTimeout(err) {
				continue
			}
			return nil, err
		}
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			readDeadline := time.Now().Add(pcpReadPoll)
			if deadline.Before(readDeadline) {
				readDeadline = deadline
			}
			if err := c.conn.SetReadDeadline(readDeadline); err != nil {
				return nil, err
			}
			count, err := c.conn.Read(buffer)
			if err != nil {
				if isTimeout(err) {
					if time.Now().Before(deadline) {
						continue
					}
					break
				}
				return nil, err
			}
			message := append([]byte(nil), buffer[:count]...)
			if match(message) {
				return message, nil
			}
		}
	}
	return nil, context.DeadlineExceeded
}

func requestPCPMapping(ctx context.Context, client *pcpGatewayClient, localAddress netip.Addr, nonce [12]byte, internalPort, externalPort uint16, externalAddress netip.Addr, lifetime uint32) (pcpMapResponse, error) {
	request := buildPCPMapRequest(localAddress, nonce, internalPort, externalPort, externalAddress, lifetime)
	message, err := client.exchange(ctx, request, func(message []byte) bool {
		if natPMPUnsupportedVersion(message) {
			return true
		}
		return len(message) >= 36 && message[0] == pcpVersion && message[1] == pcpResponseBit|pcpMapOpcode && bytes.Equal(message[24:36], nonce[:])
	})
	if err != nil {
		return pcpMapResponse{}, err
	}
	return parsePCPMapResponse(message, nonce, internalPort)
}

func newPCPOwnedMapping(nonce [12]byte, internalPort uint16, requestedLease uint32, response pcpMapResponse, started time.Time) (*pcpOwnedMapping, error) {
	owned := &pcpOwnedMapping{nonce: nonce, internalPort: internalPort, leaseSeconds: requestedLease}
	if err := applyPCPResponse(owned, response, started); err != nil {
		return nil, err
	}
	return owned, nil
}

func applyPCPResponse(owned *pcpOwnedMapping, response pcpMapResponse, started time.Time) error {
	if response.lifetime == 0 {
		return errors.New("PCP gateway returned a zero mapping lifetime")
	}
	lease := conservativeLease(owned.leaseSeconds, response.lifetime)
	owned.epoch = response.epoch
	owned.leaseSeconds = lease
	owned.mapping = Mapping{ExternalAddress: response.externalAddress, Provider: providerPCP, ExpiresAt: started.Add(time.Duration(lease) * time.Second)}
	return nil
}

func applyPCPRenewal(owned *pcpOwnedMapping, response pcpMapResponse, started time.Time) error {
	epochReset := response.epoch < owned.epoch
	if err := applyPCPResponse(owned, response, started); err != nil {
		return err
	}
	if epochReset {
		return errPCPEpochReset
	}
	return nil
}

func deletePCPMapping(ctx context.Context, client *pcpGatewayClient, nonce [12]byte, internalPort uint16) error {
	response, err := requestPCPMapping(ctx, client, client.localAddress(), nonce, internalPort, 0, netip.Addr{}, 0)
	if err != nil {
		return err
	}
	if response.lifetime != 0 {
		return errors.New("PCP gateway did not confirm mapping deletion")
	}
	return nil
}

func pcpRetryDelay(retry time.Duration, state State, owned *pcpOwnedMapping, now time.Time) time.Duration {
	if owned == nil {
		return retry
	}
	delay := retry
	if retryAfter := state.RetryAfter.Sub(now); retryAfter > delay {
		delay = retryAfter
	}
	if untilExpiry := owned.mapping.ExpiresAt.Sub(now); untilExpiry < delay {
		delay = untilExpiry
	}
	return delay
}

func buildPCPMapRequest(localAddress netip.Addr, nonce [12]byte, internalPort, externalPort uint16, externalAddress netip.Addr, lifetime uint32) []byte {
	clientAddress := localAddress.Unmap().As16()
	var suggestedAddress [16]byte
	if lifetime != 0 {
		// Each mapper requests an external address in its socket family. A
		// cross-family NAT64 or NAT46 mapping needs a separate MAP lifecycle.
		suggested := netip.IPv4Unspecified()
		if localAddress.Is6() {
			suggested = netip.IPv6Unspecified()
		}
		suggestedAddress = suggested.As16()
		if externalAddress.IsValid() {
			suggestedAddress = externalAddress.Unmap().As16()
		}
	}
	request := make([]byte, pcpMapMessageSize)
	request[0] = pcpVersion
	request[1] = pcpMapOpcode
	binary.BigEndian.PutUint32(request[4:8], lifetime)
	copy(request[8:24], clientAddress[:])
	copy(request[24:36], nonce[:])
	request[36] = pcpUDPProtocol
	binary.BigEndian.PutUint16(request[40:42], internalPort)
	binary.BigEndian.PutUint16(request[42:44], externalPort)
	copy(request[44:60], suggestedAddress[:])
	return request
}

func parsePCPMapResponse(message []byte, nonce [12]byte, internalPort uint16) (pcpMapResponse, error) {
	if natPMPUnsupportedVersion(message) {
		return pcpMapResponse{}, errors.New("PCP gateway does not support version 2")
	}
	if len(message) < pcpMapMessageSize {
		return pcpMapResponse{}, errors.New("invalid PCP MAP response size")
	}
	if message[1] != pcpResponseBit|pcpMapOpcode {
		return pcpMapResponse{}, errors.New("invalid PCP MAP response header")
	}
	if !bytes.Equal(message[24:36], nonce[:]) || message[36] != pcpUDPProtocol || binary.BigEndian.Uint16(message[40:42]) != internalPort {
		return pcpMapResponse{}, errors.New("PCP MAP response does not match request")
	}
	if err := validatePCPOptions(message[pcpMapMessageSize:]); err != nil {
		return pcpMapResponse{}, err
	}
	resultCode := message[3]
	lifetime := binary.BigEndian.Uint32(message[4:8])
	if message[0] != pcpVersion {
		if message[0] == 0 && resultCode == 1 {
			return pcpMapResponse{}, errors.New("PCP gateway does not support version 2")
		}
		return pcpMapResponse{}, errors.New("invalid PCP MAP response version")
	}
	if resultCode != 0 {
		return pcpMapResponse{}, &pcpResultError{code: resultCode, lifetime: lifetime}
	}
	response := pcpMapResponse{lifetime: lifetime, epoch: binary.BigEndian.Uint32(message[8:12])}
	if response.lifetime == 0 {
		return response, nil
	}
	externalPort := binary.BigEndian.Uint16(message[42:44])
	externalAddress := netip.AddrFrom16([16]byte(message[44:60])).Unmap()
	if externalPort == 0 || !usablePCPExternalAddress(externalAddress) {
		return pcpMapResponse{}, errors.New("PCP gateway returned an unusable external address")
	}
	response.externalAddress = netip.AddrPortFrom(externalAddress, externalPort)
	return response, nil
}

func natPMPUnsupportedVersion(message []byte) bool {
	if len(message) < 4 || message[0] != 0 || (message[1] != 0 && message[1] != 0x80) {
		return false
	}
	return binary.BigEndian.Uint16(message[2:4]) == 1
}

func validatePCPOptions(options []byte) error {
	for len(options) > 0 {
		if len(options) < 4 {
			return errors.New("invalid PCP option header")
		}
		valueLength := int(binary.BigEndian.Uint16(options[2:4]))
		paddedLength := (valueLength + 3) &^ 3
		if paddedLength > len(options)-4 {
			return errors.New("truncated PCP option")
		}
		options = options[4+paddedLength:]
	}
	return nil
}

func usablePCPExternalAddress(address netip.Addr) bool {
	if address.Is4() {
		return publiclyRoutableIPv4(address)
	}
	return address.Is6() && address.IsGlobalUnicast() && !address.IsPrivate()
}

func pcpResultText(code byte) string {
	results := [...]string{"success", "unsupported version", "not authorized", "malformed request", "unsupported opcode", "unsupported option", "malformed option", "network failure", "no resources", "unsupported protocol", "user quota exceeded", "cannot provide external address", "address mismatch", "excessive remote peers"}
	if int(code) < len(results) {
		return results[code]
	}
	return "unknown"
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func copyMapping(mapping *Mapping) *Mapping {
	if mapping == nil {
		return nil
	}
	cloned := *mapping
	return &cloned
}
