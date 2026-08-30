package iwan

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (current *generation) authenticate(ctx context.Context) (Session, error) {
	open, err := BuildOpen(current.credentials, current.options.Node.MTU)
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
				if errors.Is(parseErr, ErrProtocolDowngrade) {
					return Session{}, terminalFailure("parse OPENACK", parseErr)
				}
				return Session{}, terminalFailure("parse OPENACK", fmt.Errorf("%w: %w", ErrProtocolConfiguration, parseErr))
			default:
			}
		}
	}
}

func (current *generation) write(packet []byte) error {
	if _, err := current.connection.Write(packet); err != nil {
		return transientFailure("write UDP", fmt.Errorf("%w: %w", ErrSocketFailure, err))
	}
	return nil
}
