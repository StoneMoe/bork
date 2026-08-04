# Bork

Bork 是一个分布式优先、语音优先的跨平台房间通信软件。它不依赖固定中心服务器；在线 GUI Peer 会自动参与 discovery、UDP 打洞、单中介 bridge 和实时媒体 fan-out。

## 当前能力

- SolidJS + Wails + 系统 WebView 桌面 GUI
- 持久 Ed25519 身份与设备级单实例 lease
- 基于 256-bit `RoomSeed` 的 Base58 房间邀请
- loopback、mDNS、HTTP(S)/UDP BitTorrent Tracker discovery
- STUN、PCP、NAT-PMP、UPnP 和同步 UDP punch
- Pairwise X25519 + ChaCha20-Poly1305 控制会话
- Direct-or-one-intermediary control bridge
- 自定义可靠有序/无序 message transport
- 可靠 topology snapshot 和 fan-out assignment
- 共享房间 XChaCha20-Poly1305 实时加密
- 每包 Ed25519 sender signature，房间内其他成员不能伪造 SenderID
- 深度 2 实时 fan-out，forwarder 原样复制 ciphertext
- audio、interactive、control、background 优先级队列与单 UDP writer
- 48 kHz mono Opus、10 ms frame、DTX、adaptive bitrate、jitter buffer、PLC 和多人混音
- 可靠同步成员昵称/mute 状态，并从本地与远端 PCM 标记正在说话的成员

典型性能目标为 20–50 人，但协议不设置成员数或同时发言人数上限。实现通过 datagram、队列字节、速率、重组内存、TTL 和 deadline 等安全预算限制资源；过载时降低 bitrate、丢弃过期实时帧，而不是按人数拒绝成员。

## 信任模型

- `RoomSeed` holder 是房间成员，可以解密 group realtime data。
- Internet 观察者、Tracker 和不持有 `RoomSeed` 的节点不能读取 group realtime plaintext。
- Group datagram 的 Ed25519 signature 覆盖 header 和 ciphertext；其他房间成员即使知道共享加密 key，也不能伪造另一个 SenderID。
- Group data 不提供 forward secrecy 或成员撤销；`RoomSeed` 后续泄露可以解密之前捕获的数据。
- Pairwise control 仍使用临时 X25519 session key、AEAD sequence 和防重放。

完整设计目标与 User Stories 见 [DESIGN.md](DESIGN.md)，wire 和 transport 细节见 [PROTOCOL.md](PROTOCOL.md)，当前任务与验收指标见 [TODO.md](TODO.md)。

## 开发运行

```bash
make dev
make dev APP_ARGS="--join bork://join/..."
```

同机多实例必须使用不同身份目录：

```bash
make dev APP_ARGS="--data-dir ./tmp/peer-a"
make dev APP_ARGS="--data-dir ./tmp/peer-b --join bork://join/..."
```

一次性命令参数：

- `--join <invite>`
- `--join-file <path>`
- `--data-dir <path>`
- `--version`

没有独立 headless relay 模式；所有 GUI Peer 自动承担可用的 bridge 和 fan-out 工作。

## 配置

网络行为保存在操作系统用户配置目录的 `bork/config.yml`。首次运行写入完整默认配置：

```yaml
network:
  udp_listen: '[::]:0'
  stun_servers:
    - stun.cloudflare.com:3478
    - stun.miwifi.com:3478
  tracker_urls:
    - https://tracker.zhuqiy.com/announce
    - http://tracker.renfei.net:8080/announce
    - http://tracker.mywaifu.best:6969/announce
  port_mapping: true
```

配置位置：

- Windows：`%AppData%\bork\config.yml`
- macOS：`~/Library/Application Support/bork/config.yml`
- Linux：`${XDG_CONFIG_HOME:-~/.config}/bork/config.yml`

空 `stun_servers`/`tracker_urls` 分别禁用对应公共 provider；`port_mapping: false` 禁用网关映射。默认 Tracker 中两个使用明文 HTTP，但 Tracker 看不到 `RoomSeed`、房间状态或媒体 plaintext。

## 构建与检查

```bash
make build
go test ./...
go vet ./...
make test-frontend
```

音频使用 cgo；Windows、Linux 和 macOS 必须在对应原生 runner 构建。生成的 Wails bindings、前端产物和二进制统一位于被 Git 忽略的 `build/`。
