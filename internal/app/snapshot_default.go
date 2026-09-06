//go:build !game_proxy

package app

import "bork/internal/audio"

type AppSnapshot struct {
	Version     string       `json:"version"`
	Nickname    string       `json:"nickname"`
	Room        *RoomState   `json:"room,omitempty"`
	Audio       audio.Status `json:"audio"`
	Diagnostics Diagnostics  `json:"diagnostics"`
}
