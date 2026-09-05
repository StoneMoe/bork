# NetFilter SDK development dependency

Bork's Windows game-proxy development build is pinned to the NetFilter SDK
demo archive described by `sdk.lock.json`. The SDK payload is not tracked by
Git. The archive and extracted files live under the ignored `sdk/` directory.

## Provenance

- Version: `1.7.6.7`
- Official download: <https://netfiltersdk.com/download/nfsdk/demo/nfsdk-demo-1.7.6.7.zip>
- Official downloads page: <https://netfiltersdk.com/download.html>
- Official history: <https://netfiltersdk.com/nfsdk_history.html>
- Official license: <https://netfiltersdk.com/license.html>

The publisher does not provide a checksum manifest. The SHA-256 values in
`sdk.lock.json` were computed from the archive downloaded from the pinned
official URL on 2026-08-30. Ingestion must verify the archive before extracting
and then verify every required entry independently.

## Scope

Only the x64 WFP C API artifacts are accepted:

- `netfilter2.sys`
- `nfapi.dll` from `release_c_api/x64`
- `nfregdrv.exe` from `release_c_api/x64`
- `nfapi.h`, `nfevents.h`, and `nfdriver_data.h`
- the C pass-through sample used as API behavior evidence

The demo driver is for internal testing only. It is not a production or release
artifact and its documented connection/socket limits apply. Production builds
require separately licensed production artifacts and a separate release review.

## Development workflow

Download the pinned archive from the official URL above, then ingest and verify
it from the repository root:

```powershell
go run ./tools/netfiltersdk ingest -archive C:\path\to\nfsdk-demo-1.7.6.7.zip
go run ./tools/netfiltersdk verify
```

The ingest command verifies the archive digest before extraction and verifies
every accepted file against `sdk.lock.json`. Build and test the native backend
with the `netfilter_sdk` tag:

```powershell
go test ./...
go test -race ./internal/gameproxy/...
go test -tags netfilter_sdk ./...
go vet -tags netfilter_sdk ./...
make build TAGS=netfilter_sdk
```

The final command produces the single-file Windows application at
`build/bin/bork.exe`. Builds without `netfilter_sdk` use the unsupported factory
and do not provide Windows game interception.

## Native smoke test

The smoke test never installs or starts a driver. First install and start the pinned demo
driver as an administrator with the official SDK registration tool, then run an
elevated PowerShell with:

```powershell
$env:BORK_NETFILTER_SMOKE = "1"
$env:BORK_NETFILTER_SMOKE_DLL = (Resolve-Path "internal/gameproxy/netfilter/sdk/nfsdk/wfp/bin/release_c_api/x64/nfapi.dll")
$env:BORK_NETFILTER_SMOKE_DRIVER = "netfilter2"
go test -tags netfilter_sdk ./internal/gameproxy/netfilter -run '^TestNetFilterSDKSmoke$' -v
```

This is the only test that exercises the installed WFP driver. The ordinary
tagged test suite compiles and tests the C shim but skips driver interaction.
The TCP target uses a non-loopback local IPv4 interface so it is not bypassed;
the redirect listener itself remains on loopback. No external TCP target is needed.

## Verified ABI constraints

- The C API is selected with `_C_API` and uses `__cdecl` on Windows.
- SDK structures are packed. Go must not read packed SDK structures directly;
  the C shim converts them to Bork-owned scalar values.
- Exact process rules use `NF_RULE_EX.processName` and `nf_setRulesEx`.
- A leading PID-only `NF_ALLOW` rule excludes Bork's own IPv4 UDP sockets before
  the exact executable-path rules. Callbacks also disable filtering defensively
  when the SDK reports Bork's current process ID.
- Before initialization, Bork calls `nf_adjustProcessPriviledges` as required by
  the SDK sample. Callback process paths use `nf_getProcessNameW` first and fall
  back to `nf_getProcessNameFromKernel`.
- TCP and UDP callbacks may carry an empty diagnostic process path because the
  exact native process rule has already selected them; non-empty paths are still
  validated and matched again.
- TCP rules use `NF_INDICATE_CONNECT_REQUESTS`. The connect callback records the
  original tuple and rewrites the destination to a Bork-owned IPv4 loopback
  listener. The accepted local stream is then dialed through the existing iWAN
  netstack. NetFilter does not emulate the remote TCP peer and does not relay TCP
  payload buffers in this mode.
- Pending one-shot TCP listeners are capped at 256 bridge-wide; requests over
  the limit are rejected before allocating another listener.
- Redirect admission queries `GetExtendedTcpTable` for the client-side tuple:
  the accepted socket's remote endpoint is the table's local endpoint, and its
  local endpoint is the table's remote endpoint. Both addresses and ports must
  match a live row owned by the callback PID. The source port must also match the
  pending request. The original destination is not an ownership lookup key.
- Ownership lookup errors, missing rows, and PID mismatches reject the accepted
  socket without a port-only fallback. This is a point-in-time OS table check,
  not a kernel-bound identity proof: PID/tuple reuse and socket transfer are not
  ruled out, and a legitimate client that closes before lookup can be rejected.
- UDP rules continue to use `NF_FILTER`. The variable-length `NF_UDP_OPTIONS`
  from `udpSend` is copied and supplied to asynchronous `nf_udpPostReceive`
  calls; malformed callbacks and failed post-receive operations remain
  endpoint-local failures.
- `processName` is a UTF-16 tail mask. Bork supplies a full path without a
  wildcard and never falls back to a basename, directory, or catch-all rule.
- Callback buffers are treated as borrowed and copied before the callback
  returns. No SDK pointer may be retained by Go.
- Redirected TCP streams are ordinary loopback `net.TCPConn` values and preserve
  directional `CloseRead` and `CloseWrite`. A reset applies zero linger before
  closing only that accepted stream.
- Bork proxies IPv4 only. An IPv6 UDP callback is removed from user-mode
  filtering with `nf_udpDisableFiltering` instead of suspending the socket.
- IPv4 TCP destinations in `127.0.0.0/8` are removed from filtering before
  connect redirection so local game and anti-cheat services remain local.
- Callback validation and per-endpoint cleanup failures are reported to the
  connection event log and terminate only that endpoint. SDK statuses
  `NF_STATUS_NOT_INITIALIZED`, `NF_STATUS_IO_ERROR`, and
  `NF_STATUS_REBOOT_REQUIRED`, backend lifecycle failures, and callback panics
  stop the bridge.
- Driver registration requires administrator privileges and uses the official
  registration tool. Ordinary Bork builds never install or load demo artifacts.
- SDK initialization disables both automatic registration and automatic startup;
  the driver must already be running.

## Native acceptance still required

The headers do not define whether `processName` compares DOS or NT paths, or
whether comparison is case-sensitive. A Windows x64 smoke test must prove that
Bork's canonical full DOS path matches exactly and that an unselected sibling
executable produces no callback. No path variants or broad fallback rule may be
added before that observation.

Native acceptance must also confirm that an SDK-redirected connection appears
in the TCP owner table with the accepted socket's reversed tuple and original
callback PID. The driver-free child-process test verifies Win32 lookup and tuple
direction, not the demo driver's redirection behavior.
