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
			peerSess := remotePeer.activeSession
			if peerSess == nil || !peerSess.everAuthenticated || peerSess.path.IsDirect() != direct {
				continue
			}
			c.sendLeave(ctx, remotePeer, peerSess)
		}
	}
}

func (c *Client) sendLeave(ctx context.Context, remotePeer *RemotePeer, peerSess *PeeringSession) {
	sequence, err := peerSess.control.nextSendSequence()
	if err == nil {
		var packet []byte
		packet, err = protocol.MarshalControl(protocol.PacketLeave, c.roomTag, peerSess.sessionID, sequence, 0, peerSess.ciphers.ControlSend)
		if err == nil {
			err = c.writeControlOnPath(ctx, peerSess.path, packet)
		}
	}
	if err != nil {
		c.logger.Debug("send leave notification", "peer", remotePeer.identity.PeerID(), "error", err)
	}
}

func (c *Client) handleLeavePacketOnPath(data []byte, path Path) {
	header, err := protocol.ParseSessionHeader(data)
	if err != nil {
		return
	}
	remotePeer, peerSess := c.leaveSessionForHeader(header, path)
	if peerSess == nil {
		return
	}
	decoded, err := protocol.ParseControl(data, c.roomTag, peerSess.sessionID, peerSess.ciphers.ControlRecv)
	if err != nil || decoded.Type != protocol.PacketLeave {
		return
	}
	if !peerSess.control.commitReceived(header.Sequence) {
		return
	}
	delete(c.remotePeers, remotePeer.identity.PeerID())
	c.markPeerGraphDirty(peerSess.path.IsDirect())
	c.publishStateChange()
	c.logger.Info("remote peer left", "count", c.authenticatedRemotePeerCount())
}

func (c *Client) leaveSessionForHeader(header protocol.SessionHeader, path Path) (*RemotePeer, *PeeringSession) {
	remotePeer, peerSess, pending := c.sessionForHeader(header, path)
	if peerSess == nil || pending {
		return nil, nil
	}
	if !peerSess.everAuthenticated || !peerSess.path.SameRoute(path) {
		return nil, nil
	}
	if !peerSess.control.mayReceive(header.Sequence) {
		return nil, nil
	}
	return remotePeer, peerSess
}
