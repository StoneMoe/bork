# Bork 开发计划

## 目标

Bork 是一个分布式优先的跨平台语音通信软件。

- 每个平台交付一个可执行文件。
- GUI 使用 Wails 和系统 WebView。
- 不提供固定中心服务器模式。
- 客户端之间优先直接通信，必要时由房间内 Peer 提供转发。
- 语音追求低延迟。
- 认证后的控制和语音始终使用逐链路加密、完整性校验和防重放，不提供明文降级。

## 产品模型

### 运行模式

```text
bork
bork --join <invite>
bork --relay-peer --join <invite>
```

- 默认模式启动 GUI Peer。
- `relay-peer` 不启动 GUI，但仍是指定房间中的普通 Peer，不是公共代理。
- 所有 GUI Peer 和 `relay-peer` 都必须参与房间内的中继选举与转发，不能关闭。

### 房间与身份

- 首次运行生成持久 Ed25519 用户身份。
- PeerID 由设备公钥派生，昵称不参与身份计算。
- 整台设备上，一个持久用户身份同一时间只允许一个活动 Peer；GUI 与 `relay-peer` 通过无文件的操作系统内核 lease 共同强制该约束，切换系统用户也不能绕过，进程退出或崩溃后自动释放。
- 同机多实例联调必须使用不同的 `--data-dir` 和不同的 `identity.key`。
- 创建房间时生成 256-bit `RoomSeed`（房间种子）。
- RoomTag、Tracker 标识和准入密钥分别通过 HKDF 派生。
- 房间名称只用于显示，同名房间互不相关。
- 持有邀请的成员平权；首个 MVP 不实现管理员、踢人和成员撤销。
- 无人在线后，旧邀请仍可重新激活同一个房间。

### 公共基础设施

- 公共 STUN 用于获取 NAT 映射。
- mDNS 用于局域网发现。
- BitTorrent Tracker 用于公网 rendezvous，不承载房间状态和语音。
- 自动尝试 UPnP、NAT-PMP 和 PCP 临时端口映射。
- 所有公共 provider 都必须可替换、可禁用，并支持失败切换。

## 通信架构

每个活跃房间只使用一个 UDP socket：

```text
UDP socket
  STUN
  Discovery / Punch
  Authenticated Control
  RTP / Opus
  Relay Envelope
```

连接顺序：

1. 收集本机、IPv6、端口映射和 STUN candidates。
2. 通过 mDNS、Tracker 和邀请中的 hint 发现 Peer。
3. 双方同时进行 UDP connectivity checks 和打洞。
4. 直连成功后直接传输控制消息与语音。
5. 直连失败时，从双方都可达的房间成员中选择主 Relay 和候补 Relay。

如果所有成员都处于无法打洞的 NAT 后面，且没有可达 Peer Relay，客户端保持发现和重试，不偷偷回退到中心服务。

当前媒体处理边界：

```text
Audio Engine <-> room media.Flow <-> peer.Client.Loop
                                      |
                                RoomNetwork
                                      |
                                UDP Endpoint
```

App 只管理生命周期和状态投影，不读取、转换或转发媒体帧。每个 Client 单协程拥有 RemotePeer、PeeringSession、发送序号和接收窗口；Endpoint 使用独立的控制/实时入站队列，并以单个 10 ms 音频帧的完整 fan-out group 作为实时发送和丢弃单位。

## 安全边界

始终开启：

- Ed25519 用户身份和握手签名。
- `RoomSeed` 持有权证明。
- wire-v1 逐链路 ChaCha20-Poly1305、独立控制/语音方向密钥、序号和防重放窗口。
- Relay 路由、来源、目标和配额校验。

用户身份、邀请和局域网 Peer 认证握手已经完成。UI 只在会话 AEAD 验证成功后显示“已认证”。

当前加密是逐链路加密，不等同于未来跨 Relay 的群组 E2EE。未来 E2EE 必须使用成熟协议和库，未实现前不提供无效开关，也不允许静默降级。

## 当前实现

### 已完成

- SolidJS + Wails GUI 与单可执行文件构建。
- GUI Peer 和无界面 `relay-peer` 两种模式。
- 持久 Ed25519 身份；Windows 使用当前用户 DPAPI 保护种子。
- 版本化房间邀请、校验和与严格解析限制。
- 房间创建、导入、复制和退出。
- 单个共享 UDP socket 生命周期。
- 本机 host candidate 收集。
- 多个 STUN provider 并发探测、超时和失败降级。
- server-reflexive candidate 去重。
- 基于仅限 loopback 的内存 UDP 组播实现同设备 Peer 发现，不写发现文件。
- 基于不可枚举房间标签的 mDNS 局域网发现。
- RoomSeed 准入 MAC、Ed25519 签名和临时 X25519 握手。
- wire-v1 加密 ping/pong、成员在线状态、RTT 和超时清理。
- 48 kHz 单声道 Opus、10 ms 帧、逐链路加密实时语音包和独立媒体防重放窗口。
- 20 ms 语音预缓冲、PLC、过期包丢弃和进程内多人混音。
- 连接到成员后自动通话；支持麦克风与扬声器枚举、选择和静音。
- GUI 设置页中的 UDP/STUN 诊断。
- Wails 生成绑定和前端、平台构建产物统一位于被 Git 忽略的 `build/`。

### 尚未完成

- Tracker 和端口映射。
- Peer 发现和 UDP 打洞。
- 公网候选验证和打洞。
- 通用可靠控制层和分区成员状态合并。
- Peer Relay 选举与转发。
- RTP 互操作、AEC、噪声抑制、自动增益和音频设备热插拔恢复。

## MVP 路线

### P0：应用与身份

状态：完成。

- 单可执行文件和 Wails GUI。
- 用户身份与房间邀请。
- 删除中心服务器架构。

### P1：发现与 NAT

状态：进行中。

已完成：

- 单 UDP socket。
- host candidates。
- 公共 STUN 探测。
- 同设备发现。
- mDNS 局域网发现。
- 局域网认证握手和 ping/pong。

下一步：

1. BitTorrent Tracker 公网发现。
2. UPnP、NAT-PMP、PCP 端口映射。
3. 候选交换和 UDP 打洞。
4. 在家庭宽带、热点、CGNAT 和 IPv6 网络上记录连接矩阵。

### P2：认证控制面

1. Ed25519 签名握手和临时 X25519 会话密钥。
2. wire-v1 逐链路 AEAD、序号、防重放和会话过期。
3. 实现 ACK bitmap、RTT/RTO 和有界可靠控制层。
4. presence lease、成员状态和分区合并。

### P3：Peer Relay

1. 直连优先的路径状态机。
2. 按链路选择主 Relay 和候补 Relay。
3. 受限 Bork relay envelope、带宽和队列上限。
4. Relay 失效切换和路径迁移。

### P4：真实语音

已完成：

- 音频设备枚举、采集和播放。
- 48 kHz 单声道 Opus，默认 10 ms 帧。
- 逐链路加密实时语音包、每流 jitter buffer、PLC 和客户端混音。
- 自动通话、静音和设备选择。

下一步：

1. 两台原生设备持续通话和 mouth-to-ear 延迟测量。
2. 音频设备热插拔和休眠恢复。
3. AEC、噪声抑制和自动增益。
4. RTP 互操作、FEC 调优和直连/Relay 间无重编码切换。

### P5：硬化与发布

- AEC 手动开关和设备提示。
- parser fuzz、恶意流量和资源上限测试。
- 丢包、乱序、抖动、分区和路径迁移测试。
- Windows、macOS、Linux 原生构建和真实设备验证。

## 初始验收指标

| 指标 | 目标 |
| --- | --- |
| 软件音频管线单向延迟 p95 | <= 40 ms |
| LAN 直连 mouth-to-ear p95 | <= 60 ms |
| 默认 Opus 帧长 | 10 ms |
| 初始 jitter target | 20 ms |
| 语音发送队列 | 最多 1 帧等待 Client、1 个 fan-out group 等待 Endpoint；超限丢弃旧帧 |
| 正常房间规模 | 8 人 |
| release 文件 | 目标 <= 20 MiB，硬上限 30 MiB |
| GUI 可交互启动 | <= 1 s |

性能结果必须注明平台、网络条件、活跃流数、直连或 Relay 路径，以及 AEC 状态。

## 暂不纳入 MVP

- 固定中心服务器或托管 SaaS。
- 完整 TURN 兼容。
- DHT 和自研全球发现网络。
- 账号、好友和云同步。
- 管理员、封禁和成员撤销。
- 大型房间 SFU 和服务端混音。
- 移动端和自动更新。
