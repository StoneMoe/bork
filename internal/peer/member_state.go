package peer

import (
	"encoding/binary"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	reliableChannelMemberState = 3
	memberStateVersion         = 2
	maxNicknameRunes           = 64
	maxNicknameBytes           = 256
)

type memberState struct {
	generation    uint64
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
	if state.generation == 0 {
		return nil, errors.New("member state generation is zero")
	}
	nickname, err := NormalizeNickname(state.nickname)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 10+len(nickname))
	payload[0] = memberStateVersion
	binary.BigEndian.PutUint64(payload[1:9], state.generation)
	if state.muted {
		payload[9] |= 1
	}
	if state.playbackMuted {
		payload[9] |= 2
	}
	copy(payload[10:], nickname)
	return payload, nil
}

func decodeMemberState(payload []byte) (memberState, error) {
	if len(payload) < 10 || payload[0] != memberStateVersion {
		return memberState{}, errors.New("member state header is invalid")
	}
	state := memberState{generation: binary.BigEndian.Uint64(payload[1:9])}
	if state.generation == 0 || payload[9]&^byte(3) != 0 {
		return memberState{}, errors.New("member state fields are invalid")
	}
	state.muted = payload[9]&1 != 0
	state.playbackMuted = payload[9]&2 != 0
	rawNickname := string(payload[10:])
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
	desired.generation = c.localMemberState.generation + 1
	if desired.generation == 0 {
		desired.generation = 1
	}
	c.localMemberState = desired
	c.queueMemberStates()
}

func (c *Client) queueMemberStates() {
	payload, err := encodeMemberState(c.localMemberState)
	if err != nil {
		return
	}
	for _, peer := range c.remotePeers {
		session := peer.session
		if session == nil || !session.authenticated || session.reliable == nil || session.memberStateSentGeneration == c.localMemberState.generation {
			continue
		}
		if session.reliable.queue(reliableChannelMemberState, false, payload) != nil {
			continue
		}
		session.memberStateSentGeneration = c.localMemberState.generation
	}
}

func (c *Client) handleMemberState(sender *RemotePeer, payload []byte) {
	state, err := decodeMemberState(payload)
	if err != nil || sender == nil || sender.session == nil || state.generation <= sender.session.remoteMemberState.generation {
		return
	}
	sender.session.remoteMemberState = state
	c.publishStateChange()
}
