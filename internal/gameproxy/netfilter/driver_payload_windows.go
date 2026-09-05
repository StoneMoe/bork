//go:build windows && amd64 && game_proxy

package netfilter

import _ "embed"

const (
	netFilterSDKVersion    = "1.7.6.7"
	netFilterDLLName       = "nfapi.dll"
	netFilterDLLSHA256     = "f944b933d948c6ea51b89e790accff07bd9a48f9a9e235abd50a6f40ac7c54b0"
	netFilterDriverName    = "bork_netfilter_demo"
	netFilterDriverSHA256  = "ba886de5cbd275c8c20ccc852ef18395118e2f7d54183685f4ab9b44bcb8b48c"
	netFilterLicenseSHA256 = "e5c922f776e1e9eb4c31393ec275b4474a4e04d8aeccd7a1e49d35c7e7655840"
)

//go:embed sdk/nfsdk/wfp/bin/release_c_api/x64/nfapi.dll
var embeddedNetFilterDLL []byte

// The signed SDK bytes are unchanged; only the installed filename is different.
//
//go:embed sdk/nfsdk/wfp/bin/driver/x64/netfilter2.sys
var embeddedNetFilterDriver []byte

//go:embed sdk/nfsdk/license.rtf
var embeddedNetFilterLicense string
