//go:build windows && 386 && wintun_embed

package peer

import _ "embed"

const (
	embeddedWintunDLLSize   = 550928
	embeddedWintunDLLSHA256 = "d694fa46ab4cfebcb2632d094c7aa97278eef2f8052438621766d863ae98a931"
)

//go:embed wintun_generated/386/wintun.dll
var embeddedWintunDLL []byte

//go:embed wintun_generated/LICENSE.txt
var embeddedWintunLicense []byte
