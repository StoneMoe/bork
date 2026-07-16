//go:build production

package main

import "embed"

//go:embed all:build/frontend
var assets embed.FS
