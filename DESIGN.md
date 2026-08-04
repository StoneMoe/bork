# Bork Design

## 产品定位

Bork 是一个分布式优先、语音优先的跨平台房间通信软件。它面向熟人、团队和临时群组，不提供固定中心服务器、托管 SFU 或服务端混音。

典型目标为 20–50 人。协议不设置成员数或同时发言人数上限，但实现必须用明确的资源预算安全退化。

## 主要设计目标

### 1. 低延迟语音

- 默认 48 kHz mono Opus、10 ms frame。
- Audio 的 scheduling priority 高于 control、interactive 和 background。
- 实时数据使用 deadline；过期 frame 直接丢弃，不形成持久 backlog。
- Forwarder 不重编码、不重新加密，转发原始 ciphertext。
- Audio Engine 与 Peer 之间只有一个有界 `media.Flow` ownership boundary。

### 2. 快速 P2P discovery，但不滥用公共资源

- 同设备使用 loopback discovery，LAN 使用 mDNS，Internet 使用 Tracker。
- STUN 只探测 NAT mapping；Tracker 只做 rendezvous。
- Tracker announce、重试、candidate 数量和 HTTP/UDP 请求均有界。
- 优先 direct UDP；无法 direct 时最多经过一个共同可达 Peer。
- 没有可用路径时继续 discovery/retry，不偷偷回退到中心服务。

### 3. 支持语音之外的功能，并保护实时性能

同一个 UDP endpoint 支持不同 transport behavior：

| Lane | 目标功能 | 行为 |
| --- | --- | --- |
| audio | 语音 | 最高优先级、短 deadline、整 batch 丢弃 |
| interactive | camera、screen、自定义实时数据 | deadline、adaptive bitrate |
| control | handshake、topology、fan-out、ACK、room state | 小包、有界、公平调度 |
| background | 文件、历史同步、Tracker | 使用剩余发送机会 |

可靠功能使用 Bork 自定义 message transport，不引入 QUIC。文件由应用层继续切成 bounded chunks；不在 transport 中模拟通用 byte stream。

### 4. 控制复杂度

- 不为假想扩展建立插件框架或通用 interface。
- 新功能优先复用 `GroupDatagram` 或 reliable message channel。
- 只有一个调用方的 package boundary 应被质疑；小型 ownership value 留在其 owner package。
- 收益不显著但引入大量状态、变量或中间表示的方案默认不采用。
- 不维护任意多跳 route、BFS、warm standby 或复杂 path score。

### 5. 明确 SSOT

- `peer.Client` 单 goroutine拥有 session、topology、reliable state、fan-out assignment 和 replay state。
- Speaker 拥有自己 fan-out plan 的 SSOT；assignment 是完整版本化 replacement。
- `StateSnapshot` 是跨 goroutine projection 的 deep-copy boundary。
- Endpoint 只拥有 UDP I/O、demux、queues、rate limits 和 scheduling，不拥有 room semantics。
- App 只管理生命周期、命令和 UI snapshot，不处理逐帧媒体。

### 6. 协议应紧凑且可演进

- 所有 Bork packet 共享固定 prefix 和 1200-byte datagram ceiling。
- Packet type 明确表达行为，不用模糊 flag 组合模拟未来功能。
- Reliable transport 提供 ordered/unordered message primitives；application channel 只在具体功能需要时定义。
- Parser 必须 canonical、strict、bounded，并拒绝 trailing data。

## User Stories

### 创建与加入房间

- 作为房间创建者，我可以输入房间名并立即获得一个可复制邀请。
- 作为受邀成员，我可以粘贴邀请或通过 `--join` 启动并加入同一房间。
- 作为同机测试者，我可以用不同 `--data-dir` 启动多个独立身份。
- 作为用户，我不需要选择服务器、端口映射协议或 relay 节点。

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
- 作为房间成员，我可以展示自定义昵称和 mute 状态，并在成员列表看到当前正在说话的人。

### 可靠数据与未来功能

- 作为文本功能开发者，我可以使用 reliable ordered message channel。
- 作为 room-state 功能开发者，我可以发送版本化 full replacement，并依靠 fragmentation、ACK 和 retransmission。
- 作为文件功能开发者，我可以在 background lane 发送 bounded chunks，不阻塞 audio。
- 作为 camera/screen 功能开发者，我可以使用 interactive group datagram、deadline 和 bitrate feedback，而不复制一套 socket/crypto/fan-out。

### 失败与过载

- 作为用户，当 forwarder 或 bridge 消失时，我希望 topology 更新并重建 assignment/path。
- 作为用户，当设备或网络过载时，我希望降低 bitrate、丢弃过期 frame，而不是累积数秒延迟。
- 作为用户，当没有任何可达路径时，我希望客户端明确保持 discovering，而不是静默使用中心服务。
- 作为诊断者，我希望看到 listen address、candidates、Tracker 状态、known hints 和 direct/bridge transport。

### 隐私与身份

- 作为房间成员，我希望 group data 在 Internet 上传输时保持加密。
- 作为 sender，我希望其他 RoomSeed holder 即使能解密，也不能伪造我的 SenderID。
- 作为用户，我接受当前 group data 没有 forward secrecy 和成员撤销能力。
- 作为身份所有者，我希望同一持久身份不能被本机多个进程同时使用。

## 架构边界

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

## 连接模型

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

## 资源与规模语义

“无硬人数上限”不等于无限资源：

- 不按成员序号拒绝加入或发言。
- Session、topology、mixer source 按 TTL 清理。
- Datagram size、queue bytes、reassembly bytes、source rate 和 retained stream state 有安全预算。
- Audio bitrate 随房间规模连续调整；未来应加入 loss/queue feedback。
- 超过实时 deadline 的数据被丢弃。
- 20–50 人是待物理验证的性能目标，不是已完成声明。

## 信任模型

- `RoomSeed` 是 admission 和 group encryption 的共享秘密。
- 所有成员可以推导统一 `GroupMediaKey` 并解密 group realtime data。
- 每个 group datagram 由 claimed sender 的 Ed25519 identity 签名，其他成员不能伪造 SenderID。
- Pairwise control 使用临时 X25519 session key。
- Bridge 只能看到/处理相邻 hop 外层 control；inner control 仍属于 origin/target session。
- 昵称和 mute 是 identity session 上自声明的展示状态，不是唯一名称、权限或可信 moderation 信号。
- 当前不提供 group forward secrecy、成员撤销或历史 ciphertext 的 post-compromise protection。

## 非目标

- 固定中心服务器、托管 SaaS、中心 SFU、服务端混音
- 任意多跳 mesh routing
- 完整 TURN 或 DHT
- QUIC dependency
- 账号、好友、云同步、管理员和成员撤销
- 把当前 `GroupMediaKey` 描述为 forward-secret group E2EE
