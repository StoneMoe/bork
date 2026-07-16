//go:build !production

package main

import "embed"

//go:embed all:frontend/fallback
var assets embed.FS
