//go:build game_proxy

package iwan

import (
	"context"
	"fmt"
	"io"
	"time"
)

func (current *generation) authenticate(ctx context.Context) (Session, error) {
	open, err := BuildOpen(current.credentials, current.options.Node.MTU, current.options.Node.Encrypt)
	if err != nil {
		return Session{}, terminalFailure("build OPEN", fmt.Errorf("%w: %w", ErrProtocolConfiguration, err))
	}
	if err := current.write(open); err != nil {
		return Session{}, err
	}
	retry := time.NewTicker(current.timings.openRetry)
	defer retry.Stop()
	timeout := time.NewTimer(current.timings.authTimeout)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			return Session{}, ctx.Err()
		case <-retry.C:
			if err := current.write(open); err != nil {
				return Session{}, err
			}
		case <-timeout.C:
			return Session{}, transientFailure("authenticate", ErrAuthTimeout)
		case event := <-current.readEvents:
			if event.err != nil {
				return Session{}, transientFailure("read UDP", fmt.Errorf("%w: %w", ErrSocketFailure, event.err))
			}
			if len(event.packet) == 0 {
				continue
			}
			switch PacketType(event.packet[0]) {
			case TypeOpenReject:
				if ParseOpenReject(event.packet) == nil {
					return Session{}, terminalFailure("authenticate", ErrAuthRejected)
				}
			case TypeOpenACK:
				session, parseErr := ParseOpenACK(event.packet, current.credentials)
				if parseErr == nil {
					return session, nil
				}
				continue
			default:
			}
		}
	}
}

func (current *generation) write(packet []byte) error {
	written, err := current.connection.Write(packet)
	if err != nil {
		return transientFailure("write UDP", fmt.Errorf("%w: %w", ErrSocketFailure, err))
	}
	if written != len(packet) {
		return transientFailure("write UDP", fmt.Errorf("%w: %w", ErrSocketFailure, io.ErrShortWrite))
	}
	return nil
}
