<p align="center">
  <img src="assets/brand/appicon.png" alt="Bork" width="240">
</p>

Bork is a lightweight, low-latency, decentralized voice chat app for gaming.

## Features

- Low-latency group voice chat with no account, server, or internet connection required
- Screen sharing and file transfers
- Customizable global push-to-talk shortcuts, with desktop confirmation on Linux
- Echo cancellation, neural network noise suppression, and volume leveling across participants
- Support for Windows, macOS, and Linux

## Contributor Builds

Game proxy is an opt-in build feature: default `make build` / `make dev` builds omit its backend and UI.
Use `make build TAGS=game_proxy` / `make dev TAGS=game_proxy` to develop the game proxy version;
see the [development guide](AGENTS.md) for environment and SDK setup. Switching to the default version
does not delete existing node settings.

Download the Windows game proxy development build from a successful **Windows NetFilter Demo**
GitHub Actions run. The artifact is a single `bork.exe`, retained for 14 days, with no ZIP to extract
or companion files to download. The EXE is currently unsigned; its embedded NetFilter demo driver
retains the publisher's original signature and does not bypass Windows driver signature checks.

When starting game proxy, if Bork's dedicated driver is missing or stopped, a separate helper requests
UAC approval to install and start it. The main UI stays unelevated and does not deliberately disconnect
voice for driver installation. Canceling approval does not exit Bork; stopping the proxy does not
uninstall the driver. The original SDK license can be explicitly exported from the game proxy settings.

Windows development, test, and production builds all ship as a single EXE, without companion ZIPs or
MSIX packages; Microsoft Store is not currently a distribution target. Single-file distribution still
requires extracting the driver, DLL, and helper at runtime and does not grant a production SDK license:
the current demo is approved only for contributor testing, not production release. Other platforms keep
their native formats; macOS/Linux builds are not required to use `.exe`.
Read the [download and driver testing guide](docs/windows-netfilter-demo.md) before use. CI does not run
the helper, install or start the driver, or validate real interception behavior.
