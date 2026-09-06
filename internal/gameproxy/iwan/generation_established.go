//go:build game_proxy

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
	defer func() {
		current.expireEcho(time.Now())
		current.echoSent = time.Time{}
	}()
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	outbound := make(chan outboundEvent, 16)
	current.workers.Add(1)
	go current.readStack(workerCtx, outbound)
	echo := time.NewTimer(current.timings.echoInterval)
	defer echo.Stop()
	liveness := time.NewTimer(current.timings.liveness)
	defer liveness.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-echo.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := current.sendEcho(time.Now()); err != nil {
				return err
			}
			echo.Reset(current.timings.echoInterval)
		case <-liveness.C:
			return transientFailure("monitor liveness", ErrInactive)
		case event := <-current.readEvents:
			if err := ctx.Err(); err != nil {
				return err
			}
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

func (current *generation) sendEcho(now time.Time) error {
	current.expireEcho(now)
	if !current.echoSent.IsZero() {
		return nil
	}
	if err := current.write(current.buildEchoRequest(now)); err != nil {
		return err
	}
	// Arm only after a successful write; retain the monotonic send instant.
	current.echoSent = now
	return nil
}

func (current *generation) expireEcho(now time.Time) {
	if current.echoSent.IsZero() || now.Sub(current.echoSent) < current.timings.echoInterval {
		return
	}
	current.owner.recordLinkSample(current.id, current.echoSent.Add(current.timings.echoInterval), nil)
	current.echoSent = time.Time{}
}

func (current *generation) recordEchoResponse(sent, now time.Time) {
	current.expireEcho(now)
	if current.echoSent.IsZero() || sent.UnixMicro() != current.echoSent.UnixMicro() {
		return
	}
	delay := now.Sub(current.echoSent)
	if delay < 0 {
		return
	}
	current.echoSent = time.Time{}
	current.owner.recordLinkSample(current.id, now, new(float64(delay)/float64(time.Millisecond)))
	current.echo.CurrentDelay = uint32(delay.Microseconds())
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
