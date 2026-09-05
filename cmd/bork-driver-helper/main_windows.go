//go:build windows && amd64 && game_proxy

package main

import (
	"os"

	"bork/internal/gameproxy/netfilter"
)

func main() {
	os.Exit(netfilter.RunDriverHelper(os.Args[1:]))
}
