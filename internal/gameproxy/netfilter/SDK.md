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

## Verified ABI constraints

- The C API is selected with `_C_API` and uses `__cdecl` on Windows.
- SDK structures are packed. Go must not read packed SDK structures directly;
  the C shim converts them to Bork-owned scalar values.
- Exact process rules use `NF_RULE_EX.processName` and `nf_setRulesEx`.
- `processName` is a UTF-16 tail mask. Bork supplies a full path without a
  wildcard and never falls back to a basename, directory, or catch-all rule.
- Callback buffers are treated as borrowed and copied before the callback
  returns. No SDK pointer may be retained by Go.
- `nf_tcpClose` is the only TCP termination primitive in this API. There is no
  directional FIN or distinct reset call, so the native flow must not advertise
  `CloseRead` or `CloseWrite`; `Reset` and `Close` converge on one idempotent
  full-close operation.
- TCP interception must use `NF_OFFLINE` with `NF_FILTER` so the selected
  process does not also establish the original direct connection.
- Driver registration requires administrator privileges and uses the official
  registration tool. Ordinary Bork builds never install or load demo artifacts.

## Native acceptance still required

The headers do not define whether `processName` compares DOS or NT paths, or
whether comparison is case-sensitive. A Windows x64 smoke test must prove that
Bork's canonical full DOS path matches exactly and that an unselected sibling
executable produces no callback. No path variants or broad fallback rule may be
added before that observation.
