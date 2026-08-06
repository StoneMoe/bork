//go:build windows && amd64 && wintun_embed

package peer

import _ "embed"

const (
	embeddedWintunDLLSize   = 427552
	embeddedWintunDLLSHA256 = "e5da8447dc2c320edc0fc52fa01885c103de8c118481f683643cacc3220dafce"
)

//go:embed wintun_generated/amd64/wintun.dll
var embeddedWintunDLL []byte

//go:embed wintun_generated/LICENSE.txt
var embeddedWintunLicense []byte
