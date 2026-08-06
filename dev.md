# Bork Development

本文是 Bork 的开发入口，集中维护开发指南、技术决策、User Stories 和 Roadmap。当前 wire 格式与 transport 细节独立记录在 [PROTOCOL.md](PROTOCOL.md)。

## 开发指南

### 环境

- Go 1.25+
- Node.js、npm 与 GNU Make
- Wails v2 CLI
- 支持 cgo 的原生 C toolchain

音频依赖 cgo，Windows、Linux 和 macOS 必须在对应原生 runner 构建。

### 仓库结构

| Path | Responsibility |
| --- | --- |
| `cmd/bork` | Wails 应用入口与配置 |
| `cmd/wintun` | 固定校验 Wintun 构建资产 |
| `internal/app` | 生命周期、命令和 UI snapshot |
| `internal/audio` | 采集、播放、AEC、RNNoise、Opus 和混音 |
| `internal/networking` | discovery、endpoint、port mapping 和 room network |
| `internal/peer` | session、topology、reliable、fan-out 和 Virtual LAN |
| `internal/protocol` | application channel 常量与边界 |
| `frontend` | SolidJS 桌面界面 |
| `assets/brand` | Logo、README banner 与应用图标母版 |

### 开发运行

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

### 配置

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
  port_mapping: true
```

配置位置：

- Windows：`%AppData%\bork\config.yml`
- macOS：`~/Library/Application Support/bork/config.yml`
- Linux：`${XDG_CONFIG_HOME:-~/.config}/bork/config.yml`

空 `stun_servers` 或 `tracker_urls` 分别禁用对应公共 provider；`port_mapping: false` 禁用网关映射。默认 Tracker 中一个使用明文 HTTP，但 Tracker 看不到 `RoomSeed`、房间状态或媒体 plaintext。

### 构建与检查

```bash
make build
go test ./...
go test -race ./...
go vet ./...
make test-frontend
make typecheck-frontend
```

生成的 Wails bindings 和二进制位于被 Git 忽略的 `build/`，前端产物位于被 Git 忽略的 `internal/webassets/dist/`。品牌母版保存在 `assets/brand/`；`make build` 和 `make dev` 会把应用图标复制到 Wails workspace。

额外 Go build tags 必须通过 `TAGS` 传入，目标平台通过 `PLATFORMS` 传入；`BUILD_FLAGS` 和 `DEV_FLAGS` 不接受原始 `-tags`，`BUILD_FLAGS` 也不接受原始 `-platform`。

### 平台说明

Windows 的 `make build` 和 `make dev` 会先运行仅使用 Go 标准库的准备工具：若 `internal/peer/wintun_generated/` 的精确文件集合、size 和 SHA-256 全部匹配就离线复用；否则下载官方 Wintun 0.14.1 ZIP，固定校验 archive，只提取并校验 amd64、arm64、x86 DLL 和 LICENSE，在完整 staging tree 验证后替换无效 cache。`windows && <arch> && wintun_embed` 文件只把目标架构 DLL 和许可嵌入构建；Windows 发布物仍只有可直接运行、无需安装或 sidecar 的 `build/bin/bork.exe`。非 Windows 构建不准备 Wintun。

普通启动和非 Virtual LAN 功能不提取或加载 Wintun，也不需要管理员权限。Windows 首次启用 Virtual LAN 必须以管理员身份运行，并把 EXE 内固定 size/SHA-256 校验过的 Wintun 0.14.1 DLL 与许可写入 Windows Known Folder API 返回的 `%ProgramData%\Bork\Wintun-0.14.1\<arch>`。运行时不需要下载。

Linux Virtual LAN 需要 root 或 `CAP_NET_ADMIN`、`/dev/net/tun` 和 `ip`；macOS 需要 root、`ifconfig` 与 `route`。

屏幕共享依赖系统 WebView 提供 WebCodecs H.264；当前没有 JPEG 或软件编码 fallback。Virtual LAN 是 IPv4 Layer-3 overlay，不提供 DHCP、NAT、物理子网导出或 IPv6。

## 技术决策

### 产品定位

Bork 面向熟人、团队和临时群组，不提供固定中心服务器、托管 SFU 或服务端混音。典型目标为 20–50 人。协议不设置成员数或同时发言人数上限，但实现必须用明确的资源预算安全退化。

### 低延迟语音

- 默认 48 kHz mono Opus、10 ms frame。
- Audio scheduling priority 高于 control、interactive 和 background。
- 实时数据使用 deadline；过期 frame 直接丢弃，不形成持久 backlog。
- Forwarder 不重编码、不重新加密，转发原始 ciphertext。
- Audio Engine 与 Peer 之间只有一个有界 `media.Flow` ownership boundary。
- Capture pipeline 按 AEC、RNNoise、capture gain、VAD、Opus 的顺序处理；AEC 与 RNNoise 默认开启并可分别关闭。

### P2P discovery

- 同设备使用 loopback discovery，LAN 使用 mDNS，Internet 使用 Tracker。
- STUN 只探测 NAT mapping；Tracker 只做 rendezvous。
- Tracker announce、重试、candidate 数量和 HTTP/UDP 请求均有界。
- 优先 direct UDP；无法 direct 时最多经过一个共同可达 Peer。
- 没有可用路径时继续 discovery 和 retry，不回退到中心服务。

### Transport lanes

同一个 UDP endpoint 支持不同 transport behavior：

| Lane | 目标功能 | 行为 |
| --- | --- | --- |
| audio | 语音 | 最高优先级、短 deadline、整 batch 丢弃 |
| interactive | H.264 screen、TUN packet | deadline、丢弃过期数据 |
| control | handshake、topology、fan-out、ACK、room state | 小包、有界、公平调度 |
| background | 文件、历史同步、Tracker | 使用剩余发送机会 |

可靠功能使用 Bork 自定义 message transport，不引入 QUIC。文件由应用层继续切成 bounded chunks；不在 transport 中模拟通用 byte stream。

### 复杂度边界

- 不为假想扩展建立插件框架或通用 interface。
- 新功能优先复用 `GroupDatagram` 或 reliable message channel。
- 只有一个调用方的 package boundary 应被质疑；小型 ownership value 留在其 owner package。
- 收益不显著但引入大量状态、变量或中间表示的方案默认不采用。
- 不维护任意多跳 route、BFS、warm standby 或复杂 path score。

### Single source of truth

- `peer.Client` 单 goroutine 拥有 session、topology、reliable state、fan-out assignment 和 replay state。
- Speaker 拥有自己 fan-out plan 的 SSOT；assignment 是完整版本化 replacement。
- `StateSnapshot` 是跨 goroutine projection 的 deep-copy boundary。
- Endpoint 只拥有 UDP I/O、demux、queues、rate limits 和 scheduling，不拥有 room semantics。
- App 只管理生命周期、命令和 UI snapshot，不处理逐帧媒体。
- TUN device I/O 由有界 worker 承担；路由、远端地址和授权仍由 `peer.Client` 单 goroutine 拥有。

### 协议演进

- 所有 Bork packet 共享固定 prefix 和 1200-byte datagram ceiling。
- Packet type 明确表达行为，不用模糊 flag 组合模拟未来功能。
- Reliable transport 提供 ordered/unordered message primitives；application channel 只在具体功能需要时定义。
- Parser 必须 canonical、strict、bounded，并拒绝 trailing data。

### 架构边界

```text
GUI / App
    lifecycle, commands, snapshots
              |
Audio Engine <-> media.Flow <-> peer.Client (single owner)
                                      |
                      reliable messages / topology / fan-out
                                      |
                              RoomNetwork / Endpoint
                                      |
                  one UDP socket + prioritized writer
```

### Windows Wintun 边界

- Windows release/dev 构建先由仅使用标准库的工具验证本地生成目录的精确文件集合、size 和 SHA-256；有效时可离线构建，无效时才下载官方 Wintun 0.14.1、固定校验 archive 和目标文件，并以完整 staging tree 替换无效 cache。生成目录不进入 Git。
- 架构 build tag 只把当前目标 DLL 与许可嵌入 `bork.exe`。默认无 tag 的开发构建不依赖生成文件，启用 Virtual LAN 时明确报错。
- 运行时不访问网络。首次启用或 cache 无效时，管理员进程把嵌入文件通过同目录临时文件和 `MoveFileEx` materialize 到受保护 `%ProgramData%\Bork\Wintun-0.14.1\<arch>`。Bork、version、arch 目录逐级拒绝 reparse point 并锁定 write/delete sharing，整条 handle chain 保持到 DLL 锁定复核 hash 和绝对加载完成。
- Windows 标准 `LoadLibrary` 和 `LoadLibraryEx` 只接受物理 module path，没有可执行 memory-buffer API；`AS_DATAFILE` 和 `AS_IMAGE_RESOURCE` 不能替代 executable load 或 `GetProcAddress`。手写 reflective PE loader 会重做 relocation、import、TLS、resource、`DllMain`，绕过标准 loader 与签名模型并接近 malware tradecraft，因此明确不采用。Single-EXE distribution 仍需要一个运行时物理 DLL。
- 关闭房间或取消启用会传播 cancellation，不再开始后续 Wintun materialization，并终止平台配置命令；Wintun `CreateAdapter` 本身没有 context API，调用一旦开始只能等待驱动返回。

### 连接模型

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

- Control path 最多一个 intermediary。
- Group fan-out 最深两层。
- Speaker 根据可靠 direct-adjacency topology 计算 deterministic greedy cover。
- Listener 必须与 speaker direct，或与所选 forwarder direct。
- Indirect Virtual LAN listener 强制使用其当前 control bridge intermediary，避免媒体 greedy cover 与已认证 path 分叉。

### 资源与规模语义

“无硬人数上限”不等于无限资源：

- 不按成员序号拒绝加入或发言。
- Session、topology、mixer source 按 TTL 清理。
- Datagram size、queue bytes、reassembly bytes、source rate 和 retained stream state 有安全预算。
- 屏幕 chunk 最大 256 KiB；TUN MTU 为 1000；文件最大 1 GiB 且每个收件方向只保留一个 32 KiB 在途 chunk。
- Audio bitrate 随房间规模连续调整；未来应加入 loss 和 queue feedback。
- 超过实时 deadline 的数据被丢弃。
- 20–50 人是待物理验证的性能目标，不是已完成声明。

### 信任模型

- `RoomSeed` 是 admission 和 group encryption 的共享秘密。
- 所有成员可以推导统一 `GroupMediaKey` 并解密 group realtime data。
- 每个 group datagram 由 claimed sender 的 Ed25519 identity 签名，其他成员不能伪造 SenderID。
- Pairwise control 使用临时 X25519 session key。
- Bridge 只能看到或处理相邻 hop 外层 control；inner control 仍属于 origin/target session。
- 昵称、采集静音和播放静音是 identity session 上自声明的展示状态，不是唯一名称、权限或可信 moderation 信号。
- 当前不提供 group forward secrecy、成员撤销或历史 ciphertext 的 post-compromise protection。

### 非目标

- 固定中心服务器、托管 SaaS、中心 SFU、服务端混音
- 任意多跳 mesh routing
- 完整 TURN 或 DHT
- QUIC dependency
- UDP 端口代理、TUN DHCP/NAT、物理子网导出和 IPv6 overlay
- 账号、好友、云同步、管理员和成员撤销
- 把当前 `GroupMediaKey` 描述为 forward-secret group E2EE

## User Stories

### 创建与加入房间

- 作为房间创建者，我可以输入房间名并立即获得一个可复制邀请。
- 作为受邀成员，我可以粘贴邀请或通过 `--join` 启动并加入同一房间。
- 作为同机测试者，我可以用不同 `--data-dir` 启动多个独立身份。
- 作为用户，我不需要选择服务器、端口映射协议或 relay 节点。
- 作为 Windows 用户，我只下载一个 `bork.exe` 即可直接打开，不需要 Bork installer 或 EXE sidecar。
- 作为 Windows 用户，普通启动不写入 Wintun；只有首次启用 Virtual LAN 或受保护 cache 失效时，管理员进程才从 EXE materialize 固定校验的目标架构 DLL 与许可。

### 自动发现与建链

- 作为同一设备上的两个 Peer，我希望无需文件或手工地址即可互相发现。
- 作为同一 LAN 的成员，我希望 mDNS 自动发现并在 10 秒内完成认证。
- 作为不同网络的成员，我希望 Tracker 提供 hints，随后仍通过 RoomSeed admission、签名 Hello 和 AEAD Pong 验证对方。
- 作为 NAT 后成员，我希望客户端自动尝试 STUN、PCP、NAT-PMP、UPnP 和同步 punch。
- 作为无法 direct 的成员，我希望通过一个共同可达 Peer bridge control，而不是连接固定中心服务。

### 群组语音

- 作为 speaker，我希望只加密一次 frame，并由选定 forwarders 分担 fan-out upload。
- 作为 listener，我希望听到的 frame 能验证真实 SenderID，其他房间成员不能冒充 speaker。
- 作为房间成员，我接受所有成员共享 group decryption key，但不接受 Internet 观察者读取媒体。
- 作为 forwarder，我只转发验证通过的原始 packet，不重编码或重新加密。
- 作为多 speaker 房间成员，我不希望系统因固定人数 cap 拒绝第 N 个 speaker。
- 作为房间成员，我可以展示自定义昵称、采集/播放静音状态，并在成员列表看到当前正在说话的人。
- 作为房间成员，我默认获得回声消除和智能降噪，也可以在语音设置中分别关闭。

### 可靠数据与未来功能

- 作为文本功能开发者，我可以使用 reliable ordered message channel。
- 作为 room-state 功能开发者，我可以发送版本化 full replacement，并依靠 fragmentation、ACK 和 retransmission。
- 作为用户，我可以向一个已认证成员 offer 文件；对方选择保存位置并接受后，数据以 32 KiB stop-and-wait chunk 传输并校验 SHA-256。
- 作为屏幕分享者，我可以发送 WebCodecs H.264 Annex-B 视频；丢包后的接收方等待下一关键帧，而不是显示损坏的预测帧。
- 作为 Virtual LAN 用户，我可以显式创建 `100.64.0.0/10` TUN 地址，并通过 direct 或当前单中介 path 路由 IPv4 packet，而不是配置 UDP 端口代理。
- 作为 camera 功能开发者，我可以继续复用 interactive group datagram、deadline 和 fan-out，但当前不实现 camera。

### 失败与过载

- 作为用户，当 forwarder 或 bridge 消失时，我希望 topology 更新并重建 assignment 或 path。
- 作为用户，当设备或网络过载时，我希望降低 bitrate、丢弃过期 frame，而不是累积数秒延迟。
- 作为用户，当没有任何可达路径时，我希望客户端明确保持 discovering，而不是静默使用中心服务。
- 作为诊断者，我希望看到 listen address、candidates、Tracker 状态、known hints 和 direct/bridge transport。

### 隐私与身份

- 作为房间成员，我希望 group data 在 Internet 上传输时保持加密。
- 作为 sender，我希望其他 RoomSeed holder 即使能解密，也不能伪造我的 SenderID。
- 作为用户，我接受当前 group data 没有 forward secrecy 和成员撤销能力。
- 作为身份所有者，我希望同一持久身份不能被本机多个进程同时使用。

## Roadmap

### P0：真实设备与网络验收

- [ ] 两台原生设备关闭 STUN，在普通 IPv4 LAN 内自动认证 <=10 s
- [ ] 有线耳机进行 30 分钟双向通话与 mute/unmute 恢复
- [ ] 每方向至少记录 100 个 mouth-to-ear samples
- [ ] 验证 LAN mouth-to-ear p95 <=60 ms
- [ ] 家庭宽带、手机热点、CGNAT、IPv6 记录 direct/bridge/fan-out matrix
- [ ] 验证 PCP、NAT-PMP、UPnP mapping 建立、续租、过期清理和退出删除
- [ ] 破坏 bridge/forwarder 链路，验证 topology 更新和 assignment 重建

### P1：规模与性能

- [ ] 20、50、100 个模拟 Peer 的 topology/reliable/fan-out soak test
- [ ] 20–50 台真实设备或等价网络节点的持续 room test
- [ ] 多 speaker 同时发送时记录 CPU、内存、pps、queue drops 和 verify cost
- [ ] Benchmark Ed25519 verify 在目标平台上的 100、1000、5000 packets/s 成本
- [ ] 验证无 count-based member/speaker rejection
- [ ] 验证 realtime overload 只产生 bounded frame loss，不产生持续 latency backlog
- [ ] 验证 forwarder upload 在 fan-out plan 下分布合理
- [ ] 验证 reliable ACK-window、loss recovery 和 4 MiB budgets 的长时间稳定性

### P2：自适应媒体

- [ ] 从 endpoint queue pressure、RTT 和 packet loss 生成 media feedback
- [ ] Audio bitrate 从“按房间人数”升级为“人数 + loss + queue pressure”
- [x] 为 H.264 screen 定义 bounded fragmentation、周期关键帧和丢包后等待关键帧
- [ ] 为 interactive stream 增加 loss feedback、自适应 bitrate、FEC 和显式 keyframe request
- [ ] 验证 bitrate 调整不阻塞 10 ms audio hot path
- [ ] 评估 Ed25519 batch/hash-chain 优化；只能在不降低 sender authenticity 时采用

### P3：房间状态与文本

- [ ] 定义 presence lease 和过期规则
- [ ] 定义分区后的 room-state merge 语义
- [ ] 在 Reliable channel 上实现 text message
- [ ] 实现 history pagination 和 bounded retention
- [ ] UI 展示连接、bridge、fan-out 和 reliable diagnostics

### P4：文件传输

- [x] 定义 channel 6 control 与 background channel 7 data
- [x] 实现 32 KiB stop-and-wait、SHA-256、accept/reject/cancel 和未完成文件清理
- [ ] 实现跨 session resume
- [ ] 全局或 per-peer bandwidth budget
- [ ] 验证文件传输不会增加 audio p95 latency
- [x] 拒绝非 canonical basename，并由接收方系统对话框选择 exclusive-create 目标
- [ ] 增加磁盘剩余空间预检

### P5：Camera 与 Screen

- [x] 定义 channel 4 screen state 与 Interactive GroupDatagram signaling
- [x] 实现 WebCodecs H.264 Baseline/Main、Annex-B、分片重组和周期关键帧
- [ ] 定义 codec/profile negotiation，并评估无 WebCodecs 平台的编码方案
- [ ] 实现 loss feedback、FEC 和显式 keyframe request
- [x] 复用 group encryption、sender signature 和 fan-out assignment
- [ ] 验证 video/screen overload 不影响 audio lane

### P6：Virtual LAN

- [x] 实现 `100.64.0.0/10`、MTU 1000 的 Windows/Linux/macOS IPv4 TUN
- [x] 实现 deterministic 地址、冲突拒绝、direct/single-intermediary target routing 与 fake-TUN 双客户端测试
- [x] Windows 构建时离线复用或固定校验官方 Wintun 并按架构嵌入 EXE；运行时锁住受保护 ProgramData 目录链，首次启用或 cache 无效时写入 DLL/许可并锁定校验后绝对加载
- [ ] 在两台原生设备验证 ICMP、TCP、UDP、subnet broadcast、启停清理和 bridge churn
- [ ] 为地址冲突实现自动重选，而不只是明确报错
- [ ] 评估 IPv6 overlay；当前不导出物理 LAN subnet、不做 DHCP/NAT

### P7：Audio 质量

- [x] AEC
- [x] RNNoise noise suppression
- [ ] Automatic gain control
- [ ] Audio device hot-plug
- [ ] Sleep/wake recovery
- [ ] RTP interoperability 评估
- [ ] FEC/packet-loss 参数实测调优
- [ ] 在真实麦克风、键盘噪声和回声环境下校准 VAD threshold/release

### P8：安全与硬化

- [ ] 扩展 wire、invite、Tracker、portmap parser fuzzing
- [ ] 重组内存、queue、signature verify 和 forwarding amplification 攻击测试
- [ ] RoomSeed 泄露和恶意 room member 的威胁模型文档
- [ ] 验证 sender signature 在所有 group feature 中不可绕过
- [ ] 丢包、乱序、重复、延迟、分区和 bridge churn fault injection
- [ ] Windows、macOS、Linux 原生 build/release 验证

### 验收指标

| 指标 | 目标 |
| --- | --- |
| 软件 audio pipeline 单向 p95 | <=40 ms |
| LAN mouth-to-ear p95 | <=60 ms |
| 默认 Opus frame | 10 ms |
| 初始 jitter target | 20 ms |
| 已知 hint 的 direct 建链 | <=5 s |
| 已知 topology 的 single bridge 建链 | <=5 s |
| Fan-out assignment 重建 | <=3 s |
| 典型性能目标 | 20–50 人，无协议人数 cap |
| Reliable sender/reassembly memory | 各 <=4 MiB / Peer |
| Realtime queue | 有界、deadline/drop-oldest、无持久 backlog |
| GUI 可交互启动 | <=1 s |
| Release 文件 | 目标 <=20 MiB，硬上限 30 MiB |

性能结果必须记录平台、成员数、同时 active streams、direct/bridge、forwarder degree、bitrate、packet loss、queue drops、signature verify cost 和 AEC 状态。
