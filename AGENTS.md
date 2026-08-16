# Bork 开发指南

本文档是 Bork 的开发入口，集中记录开发说明、架构约束和产品要求。
当前线协议与传输行为以 [`internal/protocol`](internal/protocol)、[`internal/networking`](internal/networking) 和 [`internal/peer`](internal/peer) 为准。

## 开发

### 环境要求

- Go 1.26.2 或更高版本
- Node.js、npm 和 GNU Make
- Wails v2 CLI
- 支持 cgo 的原生 C 工具链

每个目标操作系统都必须使用原生构建环境；音频代码依赖 cgo。

### 仓库结构

| 路径 | 职责 |
| --- | --- |
| `cmd/bork` | 启动 Wails 应用并加载配置 |
| `internal/app` | 管理生命周期、命令和 UI 快照 |
| `internal/audio` | 采集、处理、编码、混音和播放音频 |
| `internal/networking` | 管理发现、RoomUDP、端口映射和房间网络 |
| `internal/peer` | 管理 Session、拓扑、可靠传输和扇出 |
| `internal/protocol` | 定义线协议编解码、数据包限制和密码学 |
| `frontend` | SolidJS 桌面界面 |
| `assets/brand` | Logo、README 横幅和应用图标源文件 |

### 本地运行

使用以下命令启动 Bork：

```bash
make dev
```

应用支持以下一次性启动参数：

- `--version`

### 配置

Bork 将网络配置存放在用户配置目录下的 `bork/config.yml`。首次启动时写入以下完整默认配置：

```yaml
network:
  udp_listen: '[::]:0'
  stun_servers:
    - stun.cloudflare.com:3478
    - stun.miwifi.com:3478
  tracker_urls:
    - https://bork-pex.iii.moe/announce
  port_mapping: true
```

各平台配置路径：

- Windows：`%LocalAppData%\bork\config.yml`
- macOS：`~/Library/Application Support/bork/config.yml`
- Linux：`${XDG_CONFIG_HOME:-~/.config}/bork/config.yml`

- 空的 `stun_servers` 列表会禁用公共 STUN 服务。
- 空的 `tracker_urls` 列表会禁用公共 Tracker 服务。
- 将 `port_mapping` 设为 `false` 会禁用网关端口映射。
- 默认 Tracker 可看到派生的 tracker hash、派生的 tracker peer ID、候选地址和源地址，但无法获取 `RoomSeed`、房间状态或媒体明文；tracker peer ID 会随每次入房使用的临时节点密钥变化。

### 构建与验证

执行以下命令：

```bash
make build
go test ./...
go vet ./...
make typecheck-frontend
```

- Git 会忽略 `build/` 中生成的 Wails 绑定和二进制文件。
- Git 会忽略 `internal/webassets/dist/` 中的前端产物。
- 品牌源文件位于 `assets/brand/`。
- `make build` 和 `make dev` 会将应用图标复制到 Wails 工作区。
- 通过 `TAGS` 传递额外的 Go build tags。
- 通过 `PLATFORMS` 传递目标平台。
- 不要在 `BUILD_FLAGS` 或 `DEV_FLAGS` 中直接加入 `-tags`。
- 不要在 `BUILD_FLAGS` 中直接加入 `-platform`。

## 产品边界与工程原则

### 产品范围

- Bork 面向已知联系人、团队和临时群组。
- Bork 禁止使用固定中央服务器、托管 SFU 或服务端混音，也不得依赖任何固定中央服务。
- 20 至 50 人房间只是测试目标，不代表已完成性能承诺。
- Bork 不提供无头中继模式；需要桥接和群组转发时，自动使用 GUI Peer。
- Windows 用户只需下载 `bork.exe`；程序无需安装器或 EXE 旁加载文件即可运行。

### 复杂度约束

- 拒绝在没有明确收益时增加引入更多状态、变量和圈复杂度的设计。
- 尽可能保持单一事实来源，避免在多处维护相同信息。
- 不实现通用多跳网状路由，也不维护多跳路由、BFS、热备路径或复杂路径评分。
- 不提供完整 TURN 服务、DHT、UDP 端口代理，也不使用 QUIC。
- 不提供账户、好友、云同步、管理员或成员撤销功能。

## 网络与连接

### 实现

- 基础网络层始终使用单个 UDP 端点。
- 避免 IP 分片以保持传输效率。
- 语音业务始终优先，低延迟语音是首要目标。

### 发现与 NAT 穿透

- 同一设备上的两个 Peer 通过回环发现彼此，无需文件或手动地址。
- 局域网内使用 mDNS 发现，并在 10 秒内完成认证。
- 互联网上使用 Tracker 发现不同网络中的 Peer；Tracker 只提供会合信息和候选地址提示。
- STUN 仅用于检测 NAT 映射。对 NAT 后的成员，Bork 自动尝试 STUN、PCP、NAT-PMP 和 UPnP。
- Bork 可尝试同时 UDP 打洞。
- 优先直连 UDP；直连失败时才使用下述单桥路径，并在适当时尝试将桥接 Session 升级为直连。
- 用户无需选择服务器、端口映射协议或中继节点。

### 连接模型

```text
直连：
A ---------------- B

单桥：
A -------- F -------- B

群组扇出：
             +-- L1
Speaker -- F1+-- L2
        +-- F2+-- L3
```

- 控制路径最多经过一个中间 Peer。
- 群组扇出路径最多为两跳。
- Speaker 根据可靠的直连邻接拓扑计算确定性的贪心覆盖。
- 扇出规划只选择与 Speaker 直连，或与所选 Forwarder 直连的 Listener，避免媒体覆盖路径与认证路径分离；可信成员应遵循该规划，接收端不把 Forwarder 身份作为恶意成员校验边界。

### 媒体传输

- Speaker 对每个音频帧只加密一次，由选定的 Forwarder 分担扇出上传。
- Forwarder 只发送经过验证的原始数据包，不重新编码或加密。
- 屏幕发送端使用系统 WebView 提供的 WebCodecs 生成 H.264 Annex-B 视频。
- 不提供 JPEG 或软件编码器回退。
- 单个屏幕数据块最大为 256 KiB。
- 丢包后，接收端等待下一个关键帧，不显示受损的预测帧。

### 可靠传输

- 房间状态通过可靠传输发送带版本的全量替换。
- 可靠传输提供分片、确认和重传。
- 文件使用 32 KiB 停等分块传输，并通过 SHA-256 校验。

### 资源与故障处理

没有固定成员上限不代表资源可以无限增长。

- 协议不设置房间成员数或同时发言人数上限，不得仅因房间人数或活跃 Speaker 数量拒绝成员或新 Speaker。
- 在合理超时后移除不活跃的 Session、拓扑记录和混音源。
- Forwarder 或 Bridge 停止工作时，先更新拓扑，再重建分配或路径。
- 丢弃过期帧，不得积压出数秒延迟。
- 没有可用路径时显示 `discovering` 状态，不得静默回退到中央服务。
- 诊断信息必须显示监听地址、候选地址、Tracker 状态、已知提示以及直连或桥接传输状态。

## 安全与信任

### 信任边界

- `RoomSeed` 是唯一准入凭证和 Room Datagram 加密的共享秘密。所有 `RoomSeed` 持有者权限等价，共同构成一个完全可信的安全主体。
- 邀请编码直接包含 `RoomSeed`，属于持有即授权的敏感数据。房间历史会将邀请保存在 WebView 用户配置中，剪贴板和本机用户配置目录均属于本地信任边界。
- 完成基于 `RoomSeed` 的准入和 Session 认证后，Peer 即为可信房间成员。协议不提供持有者之间的权限隔离、成员身份确认或恶意成员防护。
- Bork 假设所有 `RoomSeed` 持有者遵循协议、如实报告状态，且不会故意冒充其他临时节点、污染重放窗口或滥用转发和资源。
- `RoomTag`、发现提示和准入前收到的数据包仍是不可信的路由输入。
- 必须执行准入 MAC、transcript、路径、临时节点签名和重放检查，以拒绝不知道 `RoomSeed` 的外部人员以及捕获、重复的数据包。
- 准入后的校验、超时和资源限制用于保护线协议正确性、防止实现错误和意外过载，而不是构建恶意成员沙箱。
- 成员自行报告的昵称、采集静音和播放静音状态视为可信房间状态；Bork 不为这些字段定义名称唯一性、权限或管理语义。

### 加密与临时节点密钥

- 任何 `RoomSeed` 持有者都能派生与所有房间成员相同的 `RoomDatagramKey`，并解密 Room Datagram。
- 每次创建或加入房间时生成新的内存临时 Ed25519 密钥；离开房间后从客户端状态中丢弃。Bork 不创建 `identity.key`，不提供账户、设备身份或跨房间、离开后重入的节点连续性；同一次入房内的 Session 重握手和路径切换继续使用同一 NodeID。
- 临时 Ed25519 公钥是本次入房期间的 NodeID，用于 Session transcript、拓扑、桥接寻址、Room Datagram 签名和重放状态分区。它不代表独立安全主体，权限仅来自 `RoomSeed`。
- Room Datagram 签名将原始数据包绑定到本次入房的临时 NodeID，使 Forwarder 能在不解密的情况下验证并转发原包；该签名不构成 `RoomSeed` 持有者之间的信任边界。
- 群组数据在互联网上保持加密，互联网观察者无法读取媒体。
- 每个 Session 使用临时 X25519 密钥派生点对点控制加密密钥。
- 对于桥接流量，Bridge 会解密其相邻 Session 的外层数据包。
- `HELLO` 内层数据包未加密，因此 Bridge 可以读取。
- Bridge 无法解密端到端内层 Ping、Pong 或 Reliable 载荷。
- Room Datagram 不具备前向保密、成员撤销或旧密文的入侵后保护。不得将当前 `RoomDatagramKey` 描述为具备前向保密的群组 E2EE。

## 功能要求

### 创建和加入房间

- 作为房间创建者，我可以输入房间名称，并立即获得可复制的邀请。
- 作为受邀成员，我可以粘贴邀请加入房间。

### 群组音频与成员状态

- 作为房间成员，我可以进行低延迟群组语音通话。
- 作为房间成员，我可以设置昵称，并分别控制采集静音和播放静音。
- 作为房间成员，我可以使用默认按键 `` ` `` 或自定义单键进行全局按键说话；系统监听仅在房间内注册。
- 作为房间成员，我可以在成员列表中看到谁正在发言。
- 作为房间成员，我可以分别关闭默认开启的 AEC 和 RNNoise。

### 文件传输

- 作为发送方，我可以向已认证成员发送文件提议。
- 作为接收方，我可以选择保存位置，并在接受提议后开始接收文件。

### 屏幕共享

- 作为房间成员，我可以共享或观看其他成员的屏幕。
