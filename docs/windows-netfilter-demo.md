# Windows NetFilter demo build

Copyright (c) 2026 Bork contributors.

This is a Windows x64 development build for contributor testing, not a production
release. Windows development, test, and production builds all use the same
single `bork.exe` delivery format, without a sidecar ZIP bundle or MSIX exception.
Microsoft Store distribution is not a current target. Other platforms retain
their native formats; macOS and Linux are not expected to use `.exe` files.

The executable embeds the original signed NetFilter SDK demo driver. Only
contributor demo redistribution has been approved. Production distribution still
requires separately licensed production artifacts and a separate release review.
Single-file delivery does not grant production SDK licenses or rights to
redistribute SDK source code.

## Download

In the repository's **Actions** tab, open a successful **Windows NetFilter Demo**
run for the branch or pull request you want to test. Download the
`bork.exe` artifact directly; there is no ZIP to extract or companion file to
download. The workflow uses `actions/upload-artifact@v7` with `archive: false`,
so the filename is also the artifact name. You must be signed in to GitHub.
Artifacts expire after 14 days.

The workflow runs on pushes, pull requests, and manual dispatches. Fork pull
requests do not need secrets, but GitHub may require a maintainer to approve the
run. A pull-request artifact tests GitHub's merge commit, not just the PR's head.
Only run a build from a revision you trust, especially before installing a driver.

This workflow builds with `TAGS=game_proxy`. Default `make build` / `make dev`
omit the entire game proxy feature, including its backend, Wails API, UI and SDK
payload. They do not delete existing game proxy configuration when saving network
settings. To develop this variant, use `make build TAGS=game_proxy` or
`make dev TAGS=game_proxy` after the SDK preparation documented below.

The uploaded `bork.exe` includes the game proxy backend, original driver and API DLL,
an independently built elevation helper, and the original SDK RTF license.
Open **Settings**, then the **Game Proxy** page to access the license and explicitly
export it to a local file. The application returns the embedded original RTF
through `GetGameProxyLicense()`; a browser Blob download saves it only when you
click export. It is not automatically written beside the EXE or shipped as a
sidecar. SDK provenance and pinned digests remain in the repository's
`internal/gameproxy/netfilter/sdk.lock.json`.

Windows must have the Microsoft Edge WebView2 Runtime and Media Foundation
installed. Windows N editions may need the Media Feature Pack. This workflow's
development EXE is unsigned. The embedded driver retains the publisher's original
signature and bytes, even though its installed filename is Bork-specific. Bork
does not bypass Windows driver signature enforcement; do not disable it.

## First game proxy start

Prefer a disposable Windows x64 test machine or VM. Launch `bork.exe` normally,
without **Run as Administrator**, configure the game directories and iWAN node,
then click **Start** on the game proxy page. Start first probes the dedicated
`bork_netfilter_demo` service with ordinary user privileges:

- If missing, Bork verifies its embedded artifacts and requests UAC authorization
  through the independent helper to install and start the demo driver.
- If stopped, Bork verifies that it is the expected Bork driver before requesting
  UAC authorization to start it.
- If already running and verified, Bork uses it without an installation prompt.
- Conflicting service paths, unrecognized files, access failures, or generic SDK
  initialization errors must not trigger blind replacement or repeated elevation.

The main executable declares `asInvoker`; only the helper requests elevation.
When launched normally, the main UI remains a normal-user process and is
not restarted; Bork does not intentionally disconnect voice to install the
driver. Global push-to-talk is not guaranteed to work while Windows displays the
UAC secure desktop. Declining or canceling UAC does not quit Bork. If you stop or
cancel the proxy while authorization is pending, later approval must not resume
that canceled proxy start. Once installation has begun, cancellation may still
leave the driver installed or running; it is not a system-installation rollback.

Single-file delivery describes the download, not a file-free runtime. Bork
materializes its embedded driver, DLL, and helper for system use. The system
driver is `%WINDIR%\System32\drivers\bork_netfilter_demo.sys`; the API DLL is
`%WINDIR%\System32\bork-netfilter-demo\nfapi.dll` in a protected directory. The
helper never reads the user's node configuration or credentials and uses only
the fixed, protected system driver/DLL paths. There is no generic `netfilter2`
takeover or migration: any old manual `netfilter2` installation is left untouched.

Driver installation and service management require administrator authorization.
An already-running driver's device access policy can still restrict use; a
generic initialization failure is not proof that elevation is needed. The SDK
permits only one attached client per driver instance. Ordinary **Stop** stops the
proxy, not the system driver, and does not uninstall it.

The demo limits the number of filtered TCP connections and UDP sockets. The
publisher documents a reboot as necessary to resume filtering after reaching
that limit. Bork does not automatically reboot Windows or bypass the demo limit.

## Test Bork

After an authorized Start, check the connection log and verify both the intended
game's traffic and that unselected applications remain unaffected. Native
acceptance must separately cover first installation, reuse of a running driver,
starting a stopped driver, UAC rejection/cancellation, late authorization after
proxy cancellation, and continued main-app/voice operation. It must use a normal
user UI, not only an elevated process. These are acceptance requirements, not
claims that live native acceptance has already passed.

Report the workflow URL/commit, Windows version, reproduction steps, and relevant
connection errors. Remove credentials and private addresses from shared logs.
The application build version is also available with `bork.exe --version`.

CI first tests and builds the default application without the SDK/helper and checks
that its dependencies, generated bindings and frontend exclude the proxy feature.
It then verifies the SDK and compiles the helper before `game_proxy` tests. It checks
both Go variants, race detection for the proxy and its integration, Go vet,
frontend types, and Windows builds. Only the enabled EXE is uploaded. Builds and
tests never run the helper or install/start the demo driver. A passing workflow
is **not** evidence that installation or WFP interception works on a real machine.
The repository's
`internal/gameproxy/netfilter/SDK.md` describes the separate opt-in native smoke
test and outstanding native acceptance checks.

## Remove your test installation

Removal is a separate, manual administrator operation, not part of ordinary
Stop. First stop the proxy and close Bork and any other clients of this Bork
driver. Open a **64-bit PowerShell as Administrator** and inspect the service and
file before making changes:

```powershell
sc.exe qc bork_netfilter_demo
sc.exe query bork_netfilter_demo
Get-AuthenticodeSignature -LiteralPath "$env:WINDIR\System32\drivers\bork_netfilter_demo.sys"
Get-FileHash -LiteralPath "$env:WINDIR\System32\drivers\bork_netfilter_demo.sys" -Algorithm SHA256
```

Continue only for the Bork installation you own: the service must be a kernel
driver pointing to `%WINDIR%\System32\drivers\bork_netfilter_demo.sys` (Windows may
display the equivalent `\SystemRoot\System32\drivers\bork_netfilter_demo.sys`
or the SDK's `system32\drivers\bork_netfilter_demo.sys`),
and the file must have the original valid publisher signature and match the
`netfilter2.sys` digest in the pinned `sdk.lock.json`. A matching service name
alone is not sufficient. If ownership, path, signature, or digest is unexpected,
stop and investigate; do not delete or overwrite it. Do not operate on the old
`netfilter2` service or its files.

```powershell
sc.exe stop bork_netfilter_demo
sc.exe query bork_netfilter_demo
```

Wait until the query reports `STATE: 1 STOPPED`. An already-stopped service can
report error 1062 for `stop`; still verify `STOPPED` with `query`. If it cannot be
stopped, do not delete the service or loaded driver. Only after it is stopped:

```powershell
sc.exe delete bork_netfilter_demo
sc.exe query bork_netfilter_demo
```

Proceed only when `query` reports error **1060** (service not installed), not
merely when `delete` succeeds. If it remains marked for deletion, close clients
and service-management windows and query again; do not force deletion or reboot
automatically. Remove only the verified Bork driver and its known support files.
Check the DLL and `license.rtf` against their pinned digests, the `ownership`
marker against `driverMarker` in `driver_files_windows.go`, and that `install.lock`
is empty. Inspect the protected folder for unexpected files, hard links, or
reparse points before cleanup; leave those for manual investigation rather than
recursively deleting them.

```powershell
Remove-Item -LiteralPath "$env:WINDIR\System32\drivers\bork_netfilter_demo.sys"
Remove-Item -LiteralPath "$env:WINDIR\System32\bork-netfilter-demo\nfapi.dll"
Remove-Item -LiteralPath "$env:WINDIR\System32\bork-netfilter-demo\license.rtf"
Remove-Item -LiteralPath "$env:WINDIR\System32\bork-netfilter-demo\install.lock"
Remove-Item -LiteralPath "$env:WINDIR\System32\bork-netfilter-demo\ownership"
[System.IO.Directory]::Delete("$env:WINDIR\System32\bork-netfilter-demo", $false)
```

The final command removes only an empty directory and fails if anything remains.
Do not broaden this into recursive cleanup of System32, the Bork configuration,
or a manual `netfilter2` installation. Bork does not perform automatic uninstall
or reboot. Starting the game proxy again after removal will request installation
authorization again.

## Publisher references

- Downloads and demo limitations: <https://netfiltersdk.com/download.html>
- SDK license: <https://netfiltersdk.com/license.html>
- Driver installation: <https://netfiltersdk.com/help/nfsdk_wfp/installation.html>
