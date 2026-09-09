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

## Troubleshooting

- When members cannot find each other, the room status now identifies whether Bork is checking STUN, contacting the tracker, waiting for another member, or punching a discovered UDP address. Both members must use the exact same invitation and should use the same Bork version.
- Settings → Diagnostics can copy a report that excludes the room invitation and its secret, and can open the persistent log folder. On Windows the log is `%LocalAppData%\bork\logs\bork.log`; it rotates at 4 MiB and retains one previous copy.
- Windows builds embed the Microsoft WebView2 Evergreen Bootstrapper. If WebView2 installation still fails, use Microsoft's Evergreen Standalone Installer from the [official WebView2 download page](https://developer.microsoft.com/microsoft-edge/webview2/#download-section).
