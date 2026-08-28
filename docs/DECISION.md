# 当前的工程决策

## 网络与连接

- 基础网络层始终使用单个 UDP 端点。
- 避免 IP 分片以保持传输效率。
- 过期的实时媒体必须丢弃，不得积压形成秒级延迟。
- 额外注意reorder buffer，jitter buffer的实现，避免因为不稳定的传输或消费导致buffer引入永久性的延迟堆积。
- 暂不考虑 Mesh 路由、多跳路由、BFS、热备路径或复杂路径评分等复杂机制。
- 不提供也不使用完整的 TURN 服务、DHT。
- 不使用 QUIC。

## 发现与 NAT 穿透

- 同一设备上的两个 Peer 通过回环发现彼此，无需文件或手动地址。
- 局域网内使用 mDNS 发现，并在 10 秒内完成认证。
- 互联网上使用 Tracker 发现不同网络中的 Peer；Tracker 只提供会合信息和候选地址提示。
- STUN 仅用于检测 NAT 映射。对 NAT 后的成员，Bork 自动尝试 STUN、PCP、NAT-PMP 和 UPnP。
- Bork 可尝试同时 UDP 打洞。
- 优先直连 UDP；直连失败时才使用下述单桥路径，并在适当时尝试将桥接 Session 升级为直连。
- 用户无需选择服务器、端口映射协议或中继节点。

## 连接模型

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

## 媒体传输

- Speaker 对每个音频帧只加密一次，由选定的 Forwarder 分担扇出上传。
- Forwarder 只发送经过验证的原始数据包，不重新编码或加密。
- Windows 屏幕发送端使用 Windows Graphics Capture；系统 Video Processor MFT 将 HDR/scRGB 映射为 SDR，Media Foundation 硬件编码器生成 H.264 Annex-B 视频。
- 屏幕声音包含 Bork 进程树之外的系统输出，不在分享者本机重复播放；声音不可用时继续共享画面。
- WebView 只负责选择来源以及解码本机预览和远端画面，不参与采集或编码。
- 当前原生屏幕发送端仅支持 Windows；其他平台仍可观看屏幕分享。
- 不提供 JPEG 或软件编码器回退。
- 单个屏幕数据块最大为 256 KiB。
- 丢包后，接收端等待下一个关键帧，不显示受损的预测帧。

## 可靠传输

- 房间状态通过固定有序的 Reliable channel 发送全量替换。
- 可靠传输提供分片、确认和重传。
- 文件使用 32 KiB 停等分块传输，并通过 SHA-256 校验。

## 资源与故障处理

没有固定成员上限的设计，不代表资源可以无限增长。

- 协议不设置房间成员数或同时发言人数上限，不得仅因房间人数或活跃 Speaker 数量拒绝成员或新 Speaker。
- 在合理超时后移除不活跃的 Session、拓扑记录和混音源。
- Forwarder 或 Bridge 停止工作时，先更新拓扑，再重建分配或路径。
- 丢弃过期帧，不得积压出数秒延迟。
- 没有可用路径时显示 `discovering` 状态，不得静默回退到中央服务。
- 诊断信息必须显示监听地址、候选地址、Tracker 状态、已知提示以及直连或桥接传输状态。

## 信任边界

- `RoomSeed` 是唯一准入凭证和 Room Datagram 加密的共享秘密。所有 `RoomSeed` 持有者权限等价，共同构成一个完全可信的安全主体。
- 邀请编码直接包含 `RoomSeed`，属于持有即授权的敏感数据。房间历史会将邀请保存在 WebView 用户配置中，剪贴板和本机用户配置目录均属于本地信任边界。
- 完成基于 `RoomSeed` 的准入和 Session 认证后，Peer 即为可信房间成员。协议不提供持有者之间的权限隔离、成员身份确认或恶意成员防护。
- Bork 假设所有 `RoomSeed` 持有者遵循协议、如实报告状态，且不会故意冒充其他临时节点、污染重放窗口或滥用转发和资源。
- `RoomTag` 仅用于发现和 Tracker 路由；发现提示和准入前收到的数据包仍是不可信的路由输入。
- 必须执行准入 MAC、Session transcript、AEAD、路径和重放检查，以拒绝不知道 `RoomSeed` 的外部人员以及捕获、重复的数据包。
- 准入后的校验、超时和资源限制用于保护线协议正确性、防止实现错误和意外过载，而不是构建恶意成员沙箱。
- 成员自行报告的昵称、采集静音和播放静音状态视为可信房间状态；Bork 不为这些字段定义名称唯一性、权限或管理语义。

## 加密与临时身份

- 任何 `RoomSeed` 持有者都能派生与所有房间成员相同的 `RoomDatagramKey`，并解密 Room Datagram。
- 每次创建或加入房间时随机生成新的 16 字节 PeerID；离开房间后丢弃。Bork 不创建 `identity.key`，不提供账户、设备身份或跨房间、离开后重入的身份连续性；同一次入房内的 Session 重握手和路径切换继续使用同一 PeerID。
- PeerID 用于 Session transcript、拓扑和桥接寻址。它不是密码学身份，也不代表独立安全主体；权限仅来自 `RoomSeed`。
- 发现阶段使用带准入 MAC 的 Hello probe；probe 不参与 Session transcript。每个 Session 独占一个 SessionID、一对 Session Hello 和 X25519 临时密钥，同一 Session 的路径切换继续复用这对 Session Hello。
- Room Datagram 使用房间共享密钥执行 AEAD。Voice StreamID 等于 PeerID；每次开始或替换屏幕分享时生成新的 Screen StreamID。可信 Forwarder 验证并转发原始数据包，不重新编码或加密。
- 群组数据在互联网上保持加密，互联网观察者无法读取媒体。
- 每个 Session 使用临时 X25519 密钥派生点对点控制加密密钥。
- 对于桥接流量，Bridge 会解密其相邻 Session 的外层数据包。
- Hello probe 和 Session Hello 内层数据包未加密，因此 Bridge 可以读取。
- Bridge 无法解密端到端内层 Ping、Pong 或 Reliable 载荷。
- Room Datagram 不具备前向保密、成员撤销或旧密文的入侵后保护。不得将当前 `RoomDatagramKey` 描述为具备前向保密的群组 E2EE。
