//go:build windows && arm64 && wintun_embed

package peer

import _ "embed"

const (
	embeddedWintunDLLSize   = 222488
	embeddedWintunDLLSHA256 = "f7ba89005544be9d85231a9e0d5f23b2d15b3311667e2dad0debd344918a3f80"
)

//go:embed wintun_generated/arm64/wintun.dll
var embeddedWintunDLL []byte

//go:embed wintun_generated/LICENSE.txt
var embeddedWintunLicense []byte
