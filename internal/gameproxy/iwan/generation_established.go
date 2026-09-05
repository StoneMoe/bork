package iwan

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"bork/internal/gameproxy/netstack"
)

type outboundEvent struct {
	packet netstack.OutboundPacket
	err    error
}

func (current *generation) runEstablished(ctx context.Context) error {
	current.echo = Echo{MinimumDelay: ^uint32(0)}
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	outbound := make(chan outboundEvent, 16)
	current.workers.Add(1)
	go current.readStack(workerCtx, outbound)
	echo := time.NewTicker(current.timings.echoInterval)
	defer echo.Stop()
	liveness := time.NewTimer(current.timings.liveness)
	defer liveness.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-echo.C:
			packet := current.buildEchoRequest(time.Now())
			if err := current.write(packet); err != nil {
				return err
			}
		case <-liveness.C:
			return transientFailure("monitor liveness", ErrInactive)
		case event := <-current.readEvents:
			if event.err != nil {
				return transientFailure("read UDP", fmt.Errorf("%w: %w", ErrSocketFailure, event.err))
			}
			valid, err := current.handleInbound(event.packet)
			if err != nil {
				return err
			}
			if valid {
				resetTimer(liveness, current.timings.liveness)
			}
		case event := <-outbound:
			if event.err != nil {
				return transientFailure("read stack", fmt.Errorf("%w: %w", ErrStackFailure, event.err))
			}
			if event.packet.Generation != current.id {
				continue
			}
			current.dataPath.recordOutboundStack(event.packet.Data)
			packet, err := current.session.BuildData(event.packet.Data)
			if err != nil {
				return transientFailure("encode stack packet", fmt.Errorf("%w: %w", ErrStackFailure, err))
			}
			if err := current.write(packet); err != nil {
				return err
			}
			current.dataPath.recordOutboundNode(len(packet))
		}
	}
}

func (current *generation) readStack(ctx context.Context, events chan<- outboundEvent) {
	defer current.workers.Done()
	for {
		packet, err := current.network.LinkEndpoint().ReadPacket(ctx)
		select {
		case events <- outboundEvent{packet: packet, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (current *generation) handleInbound(packet []byte) (bool, error) {
	if len(packet) == 0 {
		return false, nil
	}
	switch PacketType(packet[0]) {
	case TypeData, TypeDataXOR:
		current.dataPath.recordInboundData(packet)
		payload, err := current.session.ParseData(packet)
		if err != nil {
			current.dataPath.inboundMalformedDatagrams.Add(1)
			return false, nil
		}
		return current.injectIPv4(payload)
	case TypeIPFragment:
		current.dataPath.recordInboundFragment(packet)
		payload, complete, err := current.reassembler.Push(packet, current.session, time.Now())
		if err != nil {
			current.dataPath.inboundMalformedDatagrams.Add(1)
			return false, nil
		}
		if !complete {
			return true, nil
		}
		current.dataPath.inboundFragmentCompletedPackets.Add(1)
		return current.injectIPv4(payload)
	case TypeEchoRequest, TypeEchoResponse, TypeClose:
		control, err := ParseControl(packet, current.session)
		if err != nil {
			return false, nil
		}
		switch control.Type {
		case TypeEchoRequest:
			response, buildErr := BuildEchoResponse(current.session, packet)
			if buildErr != nil {
				return false, nil
			}
			if err := current.write(response); err != nil {
				return false, err
			}
			return true, nil
		case TypeEchoResponse:
			if !control.Echo.Timestamp.IsZero() {
				current.recordEchoResponse(control.Echo.Timestamp, time.Now())
			}
			return true, nil
		case TypeClose:
			return true, transientFailure("peer CLOSE", ErrPeerClosed)
		default:
			return false, nil
		}
	default:
		return false, nil
	}
}

func (current *generation) buildEchoRequest(now time.Time) []byte {
	echo := current.echo
	echo.Timestamp = now
	return BuildEchoRequest(current.session, echo)
}

func (current *generation) recordEchoResponse(sent, now time.Time) {
	delay := now.UnixMicro() - sent.UnixMicro()
	if delay < 0 {
		delay = 0
	}
	current.echo.CurrentDelay = uint32(delay)
	if current.echo.CurrentDelay < current.echo.MinimumDelay {
		current.echo.MinimumDelay = current.echo.CurrentDelay
	}
	if current.echo.CurrentDelay > current.echo.MaximumDelay {
		current.echo.MaximumDelay = current.echo.CurrentDelay
	}
}

func (current *generation) injectIPv4(payload []byte) (bool, error) {
	if len(payload) == 0 || payload[0]>>4 != 4 {
		current.dataPath.inboundInvalidIPv4Packets.Add(1)
		return false, nil
	}
	err := current.network.LinkEndpoint().InjectInbound(current.id, payload)
	if err == nil {
		current.dataPath.recordInboundStack(payload)
		return true, nil
	}
	if errors.Is(err, netstack.ErrInvalidIPv4) {
		current.dataPath.inboundInvalidIPv4Packets.Add(1)
		return false, nil
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, netstack.ErrStaleGeneration) {
		return false, transientFailure("inject stack packet", fmt.Errorf("%w: %w", ErrStackFailure, err))
	}
	return false, transientFailure("inject stack packet", fmt.Errorf("%w: %w", ErrStackFailure, err))
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}
