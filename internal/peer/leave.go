package peer

import (
	"context"
	"time"

	"bork/internal/protocol"
)

const leaveWriteTimeout = time.Second

// Send bridged notifications first to give the intermediary a chance to
// forward them before it processes our direct Leave.
func (c *Client) sendLeaves() {
	ctx, cancel := context.WithTimeout(context.Background(), leaveWriteTimeout)
	defer cancel()
	for _, direct := range []bool{false, true} {
		for _, remotePeer := range c.remotePeers {
			session := remotePeer.activeSession
			if session == nil || !session.everAuthenticated || session.path.IsDirect() != direct {
				continue
			}
			c.sendLeave(ctx, remotePeer, session)
		}
	}
}

func (c *Client) sendLeave(ctx context.Context, remotePeer *RemotePeer, session *Session) {
	sequence, err := session.packetFlow.nextSendSequence()
	if err == nil {
		var packet []byte
		packet, err = protocol.MarshalControl(protocol.PacketLeave, session.id(), sequence, 0, session.ciphers.Send)
		if err == nil {
			err = c.writeControlOnPath(ctx, session.path, packet)
		}
	}
	if err != nil {
		c.logger.Debug("send leave notification", "peer", remotePeer.peerID, "error", err)
	}
}

func (c *Client) handleLeavePacketOnPath(data []byte, path Path) {
	header, err := protocol.ParseSessionHeader(data)
	if err != nil || header.Type != protocol.PacketLeave {
		return
	}
	remotePeer, session := c.leaveSessionForHeader(header, path)
	if session == nil {
		return
	}
	_, err = protocol.ParseControl(data, session.id(), session.ciphers.Receive)
	if err != nil {
		return
	}
	if !session.packetFlow.commitReceived(header.PacketSequence) {
		return
	}
	delete(c.remotePeers, remotePeer.peerID)
	c.markPeerGraphDirty(session.path.IsDirect())
	c.publishStateChange()
	c.logger.Info("remote peer left", "count", c.authenticatedRemotePeerCount())
}

func (c *Client) leaveSessionForHeader(header protocol.SessionHeader, path Path) (*RemotePeer, *Session) {
	remotePeer, session, pending := c.sessionForHeader(header, path)
	if session == nil || pending {
		return nil, nil
	}
	if !session.everAuthenticated || !session.path.SameRoute(path) {
		return nil, nil
	}
	if !session.packetFlow.mayReceive(header.PacketSequence) {
		return nil, nil
	}
	return remotePeer, session
}
