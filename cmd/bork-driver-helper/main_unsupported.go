//go:build game_proxy && (!windows || !amd64)

package main

import "os"

func main() {
	os.Exit(87)
}
