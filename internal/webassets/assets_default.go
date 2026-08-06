//go:build !production

package webassets

import "embed"

//go:embed all:fallback
var Files embed.FS
