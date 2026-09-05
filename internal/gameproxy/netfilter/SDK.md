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
- `nfregdrv.exe` from `release_c_api/x64` for SDK provenance, not distribution or runtime use
- `nfapi.h`, `nfevents.h`, and `nfdriver_data.h`
- the C pass-through sample used as API behavior evidence
- `license.rtf` for embedding unchanged in the application

The demo driver is for development and contributor testing only. The maintainer
has approved contributor demo redistribution. This is not a production or release
artifact, and the documented connection/socket limits apply. Windows development,
test, and production builds all deliver one `bork.exe`, without a sidecar ZIP or
MSIX exception; Microsoft Store distribution is not a current target. This format
does not grant production SDK licenses: production builds require separately
licensed production artifacts and a separate release review. Other platforms use
their native formats, not Windows `.exe` files.

The `Windows NetFilter Demo` GitHub Actions workflow first tests and builds the
default application without the SDK/helper, checking dependency, binding and
frontend isolation. It then downloads the locked archive, verifies it with the
ingestion tool, and builds the helper before `game_proxy` tests and builds.
After all checks pass it directly
uploads `build/bin/bork.exe` with `actions/upload-artifact@v7`, `archive: false`;
the artifact is named `bork.exe` and retained for 14 days. There is no staging or
ZIP bundle. CI never runs the helper, installs or starts the driver, and explicitly
disables the opt-in native smoke test. No repository secrets are required,
including for fork pull requests.

The GUI embeds the original signed demo driver, API DLL, independently built
helper, and original RTF license. At runtime it extracts the driver, DLL, and
helper for system use; single-file delivery does not prohibit these system files.
The downloaded development EXE is unsigned, but the embedded driver's original
bytes and publisher signature are unchanged. No signature enforcement bypass is
used. SDK headers, samples, registration utility, and the full archive are not
published as companion files. See the
[contributor instructions](../../../docs/windows-netfilter-demo.md) for download,
UAC behavior, license access, and safe manual removal.

`GetGameProxyLicense()` returns the embedded original RTF. The Settings game proxy
page exposes an explicit export action, saving through a browser Blob only on
the user's click. No license sidecar is generated at startup or packaged beside
the executable.

## Development workflow

Download the pinned archive from the official URL above, then ingest and verify
it from the repository root:

```powershell
go run ./tools/netfiltersdk ingest -archive C:\path\to\nfsdk-demo-1.7.6.7.zip
go run ./tools/netfiltersdk verify
```

The ingest command verifies the archive digest before extraction and verifies
every accepted file against `sdk.lock.json`. If the lock file adds a required
file, re-run ingest with the retained `sdk/archive/` ZIP to update an existing
installation. Build and test the entire game proxy feature with the `game_proxy` tag:

```powershell
go test ./...
make prepare-netfilter-helper
go test -tags game_proxy ./...
go test -race -tags game_proxy ./internal/gameproxy/... ./internal/app ./internal/config
go vet -tags game_proxy ./...
make typecheck-frontend TAGS=game_proxy
make build TAGS=game_proxy
```

`prepare-netfilter-helper` depends on `verify-netfilter-sdk` and compiles the
pure-Go `cmd/bork-driver-helper` with `CGO_ENABLED=0 GOOS=windows GOARCH=amd64`,
`-tags game_proxy -trimpath -ldflags '-s -w -H=windowsgui'`, into the ignored
`internal/gameproxy/netfilter/helper/bork-driver-helper.exe`. The native GUI
factory embeds that executable, so prepare it before direct tagged tests or vet.
Preparation only compiles the helper; neither builds nor tests may execute it or
install/start the driver.

`make build TAGS=game_proxy`, `make dev TAGS=game_proxy` and
`make typecheck-frontend TAGS=game_proxy` automatically depend on helper preparation.
The final build produces `build/bin/bork.exe` with no distribution sidecars.
Use Make with space-separated `TAGS`, not `BUILD_FLAGS` or `DEV_FLAGS`, to select
Go, Wails bindings and the frontend together; do not skip bindings or frontend
generation when switching variants. Make controls the internal `BORK_GAME_PROXY`
environment variable. Without `game_proxy`, the application excludes the entire
proxy implementation, iWAN/gVisor dependencies, proxy Wails API/models, UI and CSS;
it needs neither the SDK nor helper. Only enabled builds on unsupported platforms
use the unsupported factory. Only the helper is pure Go; the native GUI still
requires cgo and a native Windows toolchain.

## Administrative privileges

The publisher's [installation guide](https://netfiltersdk.com/help/nfsdk_wfp/installation.html)
requires administrative rights for driver registration and service management,
but explicitly allows nonadministrative API clients once the driver is running.
The driver's [seclevel setting](https://netfiltersdk.com/help/nfsdk_wfp/registry.html#seclevel)
can restrict that access. A restricted or already-attached device is not evidence
that a missing-driver installation is needed.

The GUI stays at normal user privileges. Start first probes `bork_netfilter_demo`
without elevation. If missing, it verifies the embedded artifacts and invokes the
independent helper with UAC consent to install and start the driver. If stopped,
the existing service and artifacts must be verified before asking the helper to
start it. If already running and verified, it is used directly. An unexpected
service path or file must fail closed, not be overwritten. Bork does not restart
the UI or intentionally disconnect voice for elevation; secure-desktop global
push-to-talk is not guaranteed.

The installed service is `bork_netfilter_demo`, with the unchanged original
`netfilter2.sys` bytes at `%WINDIR%\System32\drivers\bork_netfilter_demo.sys` and
the API DLL at `%WINDIR%\System32\bork-netfilter-demo\nfapi.dll`. The helper does
not use user node configuration or credentials, or load a DLL from a user-writable
configuration directory; driver/DLL paths are fixed and protected by System32.
There are no legacy-user migrations: old manual `netfilter2` installations remain
untouched, with no generic service takeover or uninstall.

The SDK registers the kernel image as `system32\drivers\bork_netfilter_demo.sys`.
Windows resolves that fixed driver image under SystemRoot, not the application's
working directory. Verification accepts this exact spelling case-insensitively,
as well as the equivalent absolute and `\SystemRoot\` forms; it does not expand
arbitrary relative paths, environment variables, arguments, or alternate streams.
All protected-file and ownership checks still apply.

Canceling or declining UAC does not quit the main application. Late authorization
must not resume a canceled proxy start. Once system installation begins, the
driver may remain installed or running after cancellation. Ordinary Stop stops
the proxy but does not stop or uninstall the system driver. Removal is a separate
administrator operation after verifying, stopping, and deleting only Bork's
service; it is never automatic or a reason for recursive cleanup or reboot.

`nf_adjustProcessPriviledges` returns no status and is not an elevation check.
Process-name lookup falls back to `nf_getProcessNameFromKernel`, which the
publisher documents as available without administrative privileges. Do not gate
all proxy starts on an elevated token or interpret a generic `nf_init` failure
as an elevation requirement. The in-process SDK initialization still disables
SDK auto-registration and auto-start: only the independent authorized helper
performs installation/start, before the normal-user backend attaches.

## Native smoke test

The smoke test never invokes the helper or installs/starts a driver. It requires
a separately authorized, already-installed and running Bork demo driver. Stop
the Bork proxy and close other attached clients first, without stopping the
system driver, then run from a normal 64-bit PowerShell:

```powershell
$env:BORK_NETFILTER_SMOKE = "1"
$env:BORK_NETFILTER_SMOKE_DLL = "$env:WINDIR\System32\bork-netfilter-demo\nfapi.dll"
$env:BORK_NETFILTER_SMOKE_DRIVER = "bork_netfilter_demo"
go test -tags game_proxy ./internal/gameproxy/netfilter -run '^TestNetFilterSDKSmoke$' -v
```

This is the only test that exercises the installed WFP driver. The ordinary
tagged test suite compiles and tests the C shim but skips driver interaction.
The TCP target uses a non-loopback local IPv4 interface so it is not bypassed;
the redirect listener itself remains on loopback. No external TCP target is needed.
Native acceptance must exercise the pinned driver from a non-elevated process
with its normal access policy; an elevated-only pass does not prove that case
works. Do not set `BORK_NETFILTER_SMOKE=1` in CI or ordinary test runs.

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
- Driver installation/start requires administrator authorization through the
  independent helper and only targets the verified `bork_netfilter_demo` service.
  Builds and tests never run the helper or install/start demo artifacts.
- SDK initialization disables both automatic registration and automatic startup;
  the driver must already be running before the in-process backend attaches.

## Native acceptance still required

Live native acceptance is not established by compilation, driver-free tests, or
this packaging change. It must separately cover normal-user first Start and UAC,
running/stopped service reuse, rejection/cancellation and late authorization,
unchanged voice/main-app lifecycle, protected paths, conflicting installations,
original driver signature enforcement, and leaving manual `netfilter2` untouched.

The headers do not define whether `processName` compares DOS or NT paths, or
whether comparison is case-sensitive. A Windows x64 smoke test must prove that
Bork's canonical full DOS path matches exactly and that an unselected sibling
executable produces no callback. No path variants or broad fallback rule may be
added before that observation.

Native acceptance must also confirm that an SDK-redirected connection appears
in the TCP owner table with the accepted socket's reversed tuple and original
callback PID. The driver-free child-process test verifies Win32 lookup and tuple
direction, not the demo driver's redirection behavior.
