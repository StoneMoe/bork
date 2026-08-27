package peer

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxNicknameRunes = 32
	maxNicknameBytes = 128
)

type memberState struct {
	revision      uint64
	nickname      string
	muted         bool
	playbackMuted bool
}

func NormalizeNickname(nickname string) (string, error) {
	nickname = strings.TrimSpace(nickname)
	if !utf8.ValidString(nickname) {
		return "", errors.New("nickname is not valid UTF-8")
	}
	if utf8.RuneCountInString(nickname) > maxNicknameRunes || len(nickname) > maxNicknameBytes {
		return "", errors.New("nickname is too long")
	}
	for _, value := range nickname {
		if unicode.IsControl(value) {
			return "", errors.New("nickname contains a control character")
		}
	}
	return nickname, nil
}

func encodeMemberState(state memberState) ([]byte, error) {
	nickname, err := NormalizeNickname(state.nickname)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 1+len(nickname))
	if state.muted {
		payload[0] |= 1
	}
	if state.playbackMuted {
		payload[0] |= 2
	}
	copy(payload[1:], nickname)
	return payload, nil
}

func decodeMemberState(payload []byte) (memberState, error) {
	if len(payload) < 1 {
		return memberState{}, errors.New("member state header is invalid")
	}
	state := memberState{}
	if payload[0]&^byte(3) != 0 {
		return memberState{}, errors.New("member state fields are invalid")
	}
	state.muted = payload[0]&1 != 0
	state.playbackMuted = payload[0]&2 != 0
	rawNickname := string(payload[1:])
	nickname, err := NormalizeNickname(rawNickname)
	if err != nil || nickname != rawNickname {
		return memberState{}, errors.New("member state nickname is invalid")
	}
	state.nickname = nickname
	return state, nil
}

func (c *Client) SetLocalMemberState(nickname string, muted, playbackMuted bool) error {
	nickname, err := NormalizeNickname(nickname)
	if err != nil {
		return err
	}
	c.memberStateMu.Lock()
	if c.desiredMemberState.nickname == nickname && c.desiredMemberState.muted == muted && c.desiredMemberState.playbackMuted == playbackMuted {
		c.memberStateMu.Unlock()
		return nil
	}
	c.desiredMemberState.nickname = nickname
	c.desiredMemberState.muted = muted
	c.desiredMemberState.playbackMuted = playbackMuted
	c.memberStateMu.Unlock()
	select {
	case c.memberStateUpdates <- struct{}{}:
	default:
	}
	return nil
}

func (c *Client) applyDesiredMemberState() {
	c.memberStateMu.Lock()
	desired := c.desiredMemberState
	c.memberStateMu.Unlock()
	if c.localMemberState.nickname == desired.nickname && c.localMemberState.muted == desired.muted && c.localMemberState.playbackMuted == desired.playbackMuted {
		return
	}
	desired.revision = c.localMemberState.revision + 1
	c.localMemberState = desired
	c.queueMemberStates()
}

func (c *Client) queueMemberStates() {
	payload, err := encodeMemberState(c.localMemberState)
	if err != nil {
		return
	}
	for _, peer := range c.remotePeers {
		activeSession := peer.activeSession
		if activeSession == nil || !activeSession.authenticated || activeSession.reliable == nil || activeSession.memberStateSentRevision == c.localMemberState.revision {
			continue
		}
		if activeSession.reliable.queue(reliableChannelMemberState, payload) != nil {
			continue
		}
		activeSession.memberStateSentRevision = c.localMemberState.revision
	}
}

func (c *Client) handleMemberState(sender *RemotePeer, payload []byte) {
	state, err := decodeMemberState(payload)
	if err != nil || sender == nil || state == sender.memberState {
		return
	}
	sender.memberState = state
	c.publishStateChange()
}
