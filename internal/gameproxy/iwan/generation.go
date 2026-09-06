//go:build game_proxy

package iwan

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"bork/internal/gameproxy/netstack"
)

const udpSocketBuffer = 4 * 1024 * 1024

type generation struct {
	id          uint64
	options     Options
	credentials Credentials
	timings     runtimeTimings
	owner       *Supervisor
	connection  *net.UDPConn
	network     *netstack.Stack
	session     Session
	reassembler *Reassembler
	dataPath    *dataPathCounters
	echo        Echo
	echoSent    time.Time
	readEvents  chan datagramEvent
	workers     sync.WaitGroup
	cancelRead  context.CancelFunc
}

type datagramEvent struct {
	packet []byte
	err    error
}

func (current *generation) run(ctx context.Context) (runErr error) {
	connection, err := current.dial(ctx)
	if err != nil {
		return transientFailure("connect UDP", fmt.Errorf("%w: %w", ErrSocketFailure, err))
	}
	current.connection = connection
	readCtx, cancelRead := context.WithCancel(ctx)
	current.cancelRead = cancelRead
	current.readEvents = make(chan datagramEvent, 16)
	current.workers.Add(1)
	go current.readLoop(readCtx)
	defer func() {
		if errors.Is(runErr, context.Canceled) && current.session.Address.IsValid() {
			if _, closeErr := current.connection.Write(BuildClose(current.session)); closeErr != nil {
				runErr = errors.Join(runErr, fmt.Errorf("send CLOSE: %w", closeErr))
			}
		}
		current.cancelRead()
		if closeErr := current.connection.Close(); closeErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close UDP: %w", closeErr))
		}
		if current.network != nil {
			current.owner.deactivate(current.id, current.network)
			current.network.Close()
		}
		current.workers.Wait()
	}()

	session, err := current.authenticate(ctx)
	if err != nil {
		return err
	}
	current.session = session
	effectiveMTU := current.options.Node.MTU
	if session.MTU >= netstack.MinMTU && session.MTU < effectiveMTU {
		effectiveMTU = session.MTU
	}
	current.session.MTU = effectiveMTU
	network, err := netstack.New(netstack.Config{
		Address: session.Address, MTU: uint32(effectiveMTU),
		QueueSize: current.options.QueueSize, Generation: current.id,
	})
	if err != nil {
		return terminalFailure("configure stack", fmt.Errorf("%w: %w", ErrProtocolConfiguration, err))
	}
	current.network = network
	current.reassembler = NewReassembler()
	if !current.owner.activate(activation{id: current.id, network: network, address: session.Address, mtu: effectiveMTU}) {
		return context.Canceled
	}
	return current.runEstablished(ctx)
}

func (current *generation) dial(ctx context.Context) (*net.UDPConn, error) {
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", current.options.Node.Server)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", current.options.Node.Server, err)
	}
	var lastErr error
	for _, address := range resolvedIPv4Addresses(addresses) {
		remote := netip.AddrPortFrom(address, current.options.Node.Port).String()
		connection, dialErr := (&net.Dialer{}).DialContext(ctx, "udp4", remote)
		if dialErr != nil {
			lastErr = dialErr
			continue
		}
		udpConnection, ok := connection.(*net.UDPConn)
		if !ok {
			closeErr := connection.Close()
			return nil, errors.Join(fmt.Errorf("unexpected connection type %T", connection), closeErr)
		}
		_ = udpConnection.SetReadBuffer(udpSocketBuffer)
		_ = udpConnection.SetWriteBuffer(udpSocketBuffer)
		return udpConnection, nil
	}
	if lastErr == nil {
		lastErr = errors.New("host resolved to no IPv4 addresses")
	}
	return nil, lastErr
}

func resolvedIPv4Addresses(addresses []netip.Addr) []netip.Addr {
	resolved := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if address.Is4() {
			resolved = append(resolved, address)
		}
	}
	return resolved
}

func (current *generation) readLoop(ctx context.Context) {
	defer current.workers.Done()
	buffer := make([]byte, fragmentOutputSize+fragmentHeaderSize)
	for {
		read, err := current.connection.Read(buffer)
		if err != nil {
			select {
			case current.readEvents <- datagramEvent{err: err}:
			case <-ctx.Done():
			}
			return
		}
		event := datagramEvent{packet: append([]byte(nil), buffer[:read]...)}
		select {
		case current.readEvents <- event:
		case <-ctx.Done():
			return
		}
	}
}
