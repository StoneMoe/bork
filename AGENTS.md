# Bork Development Guide

This document is the development entry point for Bork.
It contains development instructions, technical decisions, and user requirements.
For current wire and transport behavior, refer to [`internal/protocol`](internal/protocol), [`internal/networking`](internal/networking), and [`internal/peer`](internal/peer).

## Development Guide

### Required Software

- Go 1.25 or later
- Node.js, npm, and GNU Make
- Wails v2 CLI
- A native C toolchain that supports cgo

Use a native build runner for each target operating system.
The audio code uses cgo.

### Repository Layout

| Path | Function |
| --- | --- |
| `cmd/bork` | Starts the Wails application and loads its configuration |
| `internal/app` | Controls the lifecycle, commands, and UI snapshots |
| `internal/audio` | Captures, processes, encodes, mixes, and plays audio |
| `internal/networking` | Controls discovery, RoomUDP, port mapping, and the room network |
| `internal/peer` | Controls sessions, topology, reliable transport, and fan-out |
| `internal/protocol` | Defines wire codecs, packet limits, and cryptography |
| `frontend` | Contains the SolidJS desktop UI |
| `assets/brand` | Contains the logo, README banner, and source application icons |

### Run in Development

Run Bork with one of these commands:

```bash
make dev
make dev APP_ARGS="--join bork://join/..."
```

Give each local instance a different identity directory:

```bash
make dev APP_ARGS="--data-dir ./tmp/peer-a"
make dev APP_ARGS="--data-dir ./tmp/peer-b --join bork://join/..."
```

The application accepts these one-time options:

- `--join <invite>`
- `--join-file <path>`
- `--data-dir <path>`
- `--version`

Bork has no headless relay mode.
Bork automatically uses GUI Peers for bridge and group-forwarding work.

### Configuration

Bork stores network settings in `bork/config.yml` in the user configuration directory.
On the first start, Bork writes this full default configuration:

```yaml
network:
  udp_listen: '[::]:0'
  stun_servers:
    - stun.cloudflare.com:3478
    - stun.miwifi.com:3478
  tracker_urls:
    - https://tracker.zhuqiy.com/announce
    - http://tracker.renfei.net:8080/announce
  port_mapping: true
```

Use these configuration paths:

- Windows: `%AppData%\bork\config.yml`
- macOS: `~/Library/Application Support/bork/config.yml`
- Linux: `${XDG_CONFIG_HOME:-~/.config}/bork/config.yml`

An empty `stun_servers` list disables public STUN providers.
An empty `tracker_urls` list disables public Tracker providers.
Set `port_mapping` to `false` to disable gateway port mapping.

One default Tracker uses unencrypted HTTP.
A Tracker can see the derived tracker hash and derived tracker peer ID.
A Tracker can also see candidate addresses and source addresses.
It cannot access the `RoomSeed`, room state, or media plaintext.

### Build and Verify

Run these commands:

```bash
make build
go vet ./...
make typecheck-frontend
make test-frontend
```

Git ignores generated Wails bindings and binaries in `build/`.
Git also ignores frontend output in `internal/webassets/dist/`.
Source brand files are in `assets/brand/`.
The `make build` and `make dev` commands copy the application icon to the Wails workspace.

Pass additional Go build tags through `TAGS`.
Pass target platforms through `PLATFORMS`.
Do not put a raw `-tags` option in `BUILD_FLAGS` or `DEV_FLAGS`.
Do not put a raw `-platform` option in `BUILD_FLAGS`.

### Platform Requirements

#### Screen Sharing

Screen sharing uses WebCodecs H.264 from the system WebView.
Bork has no JPEG fallback and no software encoder fallback.

## Technical Decisions

### Product Scope

Bork supports known contacts, teams, and temporary groups.
It NEVER use a fixed central server, a hosted SFU, or server-side mixing.
The expected room size is 20 to 50 members, but the protocol does not set a member limit or a concurrent-speaker limit.

### Network Design Goal

The base networking layer should use a single UDP endpoint.
The base networking should only provide a multiplex style virtual link for upper network layers.
Each networking layer should ignorance about upper layer like the OSI model design.
Keep our networking impl simple, clean and clear duty boundary.
Avoid IP fragmentation for efficient.

### Audio Goal

- Low-Latency Audio is a MUST.
- AEC and RNNoise are on by default. A user can turn off each function separately.

### P2P

- Use loopback discovery on one device.
- Use mDNS discovery on a LAN.
- Use Tracker discovery on the Internet.
- Use STUN for NAT mapping detect only.
- Use Tracker for rendezvous information only.
- Use direct UDP first. If direct UDP fails, use no more than one intermediary Peer.

### Complexity Limits

- Reject a design that adds state or variables without a clear benefit.
- Do not maintain multi-hop routes, BFS, warm standby paths, or complex path scores.
- Single Source of Truth is perfered when possible, good for maintainability. avoid duplicate samething everywhere

### Connection Model

```text
direct:
A ---------------- B

single bridge:
A -------- F -------- B

group fan-out:
             +-- L1
Speaker -- F1+-- L2
        +-- F2+-- L3
```

- A control path has no more than one intermediary.
- A group fan-out path has no more than two hops.
- The speaker calculates a deterministic greedy cover from reliable direct-adjacency topology.
- A listener must be direct to the speaker or direct to the selected forwarder.
- This rule prevents a split between the media cover and the authenticated path.

### Resource and Scale Rules

The absence of a fixed member limit does not permit unbounded resources.

- Do not reject a member because of the number of room members.
- Do not reject a speaker because of the number of active speakers.
- Remove inactive Sessions, topology records, and mixer sources after reasoning timeout.
- A screen chunk has a maximum size of 256 KiB.
- A room size of 20 to 50 members is a test target. It is not a completed performance claim.

### Trust Model

- `RoomSeed` is the shared secret for admission and group datagram encryption.
- A Peer that completes RoomSeed-based admission, identity-signature validation, and Session authentication is a fully trusted room member.
- Bork assumes trusted room members follow the protocol, report their own state honestly, and do not intentionally abuse forwarding or resources.
- `RoomTag`, discovery hints, and packets received before admission remain untrusted routing input.
- Identity, transcript, path, signature, and replay checks remain mandatory to reject outsiders and captured or duplicated network packets.
- Post-admission validation, timeouts, and resource bounds protect wire correctness, implementation faults, and accidental overload. They are not a malicious-member sandbox.
- All room members can derive the same `GroupDatagramKey` and decrypt group datagrams.
- Each group datagram has an Ed25519 signature that binds its `SenderID` to the sender's identity key.
- Each Session uses ephemeral X25519 keys to derive pairwise control encryption keys.
- For bridged traffic, a bridge decrypts the outer packet for its adjacent Session.
- A bridge can read an inner `SESSION_HELLO` because that packet is not encrypted.
- A bridge cannot decrypt the body of an inner Ping, Pong, or Reliable packet.
- A member's self-reported nickname, capture-mute state, and playback-mute state are accepted as trusted room state.
- Bork does not define unique names, permissions, or moderation semantics for those fields.
- Group data does not have forward secrecy, member revocation, or post-compromise protection for old ciphertext.

### Out of Scope

- Bork does not provide a fixed central server, hosted SaaS, central SFU, or server-side mixing.
- Bork does not provide general multi-hop mesh routing.
- Bork does not provide a full TURN service or DHT.
- Bork does not use QUIC.
- Bork does not provide UDP port proxying.
- Bork does not provide accounts, friends, cloud synchronization, administrators, or member revocation.
- Do not describe the current `GroupDatagramKey` as forward-secret group E2EE.

## User Requirements

### Create and Join a Room

- A room creator can enter a room name.
- Bork immediately gives the creator an invitation.
- The creator can copy the invitation.
- An invited member can paste an invitation or use `--join` to join the room.
- A local tester can use different `--data-dir` values for independent identities.
- A user does not select a server, port-mapping protocol, or relay node.
- A Windows user downloads only `bork.exe`.
- The file runs without a Bork installer or an EXE sidecar.

### Discover and Connect to Peers

- Two Peers on one device can find each other without a file or a manual address.
- Members on one LAN can use mDNS and complete authentication in 10 seconds or less.
- A Tracker can give hints to members on different networks.
- For a member behind NAT, Bork can automatically try STUN, PCP, NAT-PMP, and UPnP.
- Bork can also try simultaneous UDP hole punching.
- If a direct connection fails, Bork can use one intermediary Peer for control traffic.
- Bork should try upgrade a bridged session to direct connect when appropriate.
- Bork should never rely on any fixed central service.

### Use Group Audio

- A speaker encrypts a frame one time.
- Selected forwarders share the fan-out upload work.
- A forwarder sends only a verified original packet.
- A forwarder does not encode or encrypt the packet again.
- A listener can verify the real `SenderID` of an audio frame.
- Another room member cannot impersonate the speaker.
- A room member accepts that all members share the group decryption key.
- An Internet observer cannot read the media.
- Bork does not reject a new speaker because of a fixed speaker limit.
- A room member can set a nickname.
- The member can independently set capture mute and playback mute.
- The member list shows the members who are speaking.
- Echo cancellation and noise reduction are on by default.
- A room member can turn off each function separately.

### Use Reliable Data and Future Functions

- A text function can use a reliable ordered message channel.
- A room-state function can send a versioned full replacement through reliable transport.
- Reliable transport provides fragmentation, acknowledgments, and retransmission.
- A user can offer a file to an authenticated member.
- The receiver selects a save location and accepts the offer.
- Bork transfers the file in 32 KiB stop-and-wait chunks and verifies SHA-256.
- A screen sender can send WebCodecs H.264 Annex-B video.
- After packet loss, the receiver waits for the next key frame.
- The receiver does not display a damaged predicted frame.
- A future camera function can reuse `GroupVideo`, deadlines, and fan-out.
- The current implementation does not define a camera subtype.

### Handle Failure and Overload

- If a forwarder or bridge stops, update the topology.
- Then rebuild the assignment or path.
- Drop expired frames.
- Do not collect a backlog that adds multiple seconds of delay.
- If no path is available, show the discovering state.
- Do not silently use a central service.
- Diagnostics show the listen address, candidates, Tracker state, known hints, and direct or bridge transport.

### Protect Privacy and Identity

- Group data stays encrypted on the Internet.
- A `RoomSeed` holder can decrypt group data.
- The holder cannot forge another `SenderID`.
- A user accepts that current group data has no forward secrecy or member revocation.
- Only one local process can use one persistent identity at a time.
