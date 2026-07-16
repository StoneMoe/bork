# Bork

Bork 是一个分布式优先的轻量语音通信软件。每个平台提供一个可执行文件，不依赖固定的中心服务器。

## 当前状态

已实现：

- 基于 SolidJS、Wails 和系统 WebView 的桌面界面
- 持久化 Ed25519 用户身份；Windows 使用当前用户 DPAPI 保护私钥种子，其他平台校验种子与派生公钥的一致性
- 基于 256-bit `RoomSeed`（房间种子）的房间邀请
- 创建、导入、复制和离开房间
- 单个共享 UDP socket
- 本机地址候选收集
- 通过多组公共 STUN 探测公网映射
- 通过仅限 loopback 的 UDP 组播在内存中发现同一设备上的同房间 Peer，不写发现文件
- 通过 mDNS 自动发现局域网内的同房间 Peer
- RoomSeed 准入 MAC、Ed25519 身份签名和临时 X25519 链路握手
- 认证后的 ping/pong、成员在线状态和 RTT
- 48 kHz 单声道 Opus 语音、逐链路加密实时 UDP 包、20 ms 初始抖动缓冲和多人混音
- 连接到成员后自动通话；支持麦克风与扬声器枚举、选择和静音
- 无界面 `relay-peer` 运行模式骨架
- 所有 Peer 固定参与后续的房间内中继选举与转发

尚未实现：

- BitTorrent Tracker 公网成员发现
- UPnP、NAT-PMP、PCP 端口映射
- 公网 UDP 打洞
- 完整可靠控制层和房间状态同步
- Peer Relay 实际转发
- RTP 互操作、AEC、噪声抑制、自动增益和音频设备热插拔恢复

当前版本由 App 管理 GUI、CLI、身份 lease、房间和语音生命周期，不参与逐帧媒体处理。WebView 通过命令 RPC 和单一版本化快照读取状态；后端将变更通知合并后只发送 revision，前端再拉取最新快照。每个房间使用一个 `peer.Client` 调度 RemotePeer、PeeringSession 和共享 UDP endpoint；房间级 `media.Flow` 是 Peer 与 Audio Engine 之间唯一的有界媒体交接。Client 单协程拥有会话、路径探测、序号和防重放状态，Endpoint 分离控制与实时入站队列，并以单个 10 ms 音频帧的 fan-out group 作为实时发送和丢弃单位。wire-v1 对认证后的控制和语音强制使用逐链路 ChaCha20-Poly1305，不提供明文或 MAC-only 降级；语音包不重传。

## 环境要求

- Go 1.25+
- 支持 cgo 的 C 编译器
- Node.js 20.19+
- Wails CLI 2.11
- GNU Make
- Windows：WebView2
- Linux：GTK3 和 WebKit2GTK

## 开发运行

启动 GUI：

```bash
make dev
```

预先载入房间邀请：

```bash
make dev APP_ARGS="--join bork://join/..."
```

同一台设备启动两个实例联调时，必须为第二个实例使用独立的数据目录，使其拥有不同的用户身份：

```bash
make dev APP_ARGS="--data-dir ./tmp/peer-a"
make dev APP_ARGS="--data-dir ./tmp/peer-b --join bork://join/..."
```

用户身份保存在 `identity.key`。整台设备上同一身份只允许一个 Bork 实例运行；更换系统用户或复制相同的 `identity.key` 到其他数据目录都不会绕过该限制。身份 lease 由操作系统内核对象持有，不创建锁文件，进程退出或崩溃后自动释放。

启动无界面房间节点：

```bash
go run . --relay-peer --join-file invite.txt
```

常用网络参数：

```bash
go run . --udp-listen "[::]:0"
go run . --stun-servers "stun.cloudflare.com:3478,stun.l.google.com:19302"
go run . --stun-servers=""
```

最后一条命令会禁用 STUN。公共 STUN 能看到请求来源 IP，但不承载房间状态或语音。

## 构建

```bash
make build
```

Wails RPC、model 和 runtime 绑定在构建或测试前生成到 `build/wailsjs/`。所有生成物均被 Git 忽略：

- `build/wailsjs/`：Wails RPC、model 和 runtime 绑定
- `build/frontend/`：SolidJS/Vite 前端产物
- `build/bin/`：开发和 release 二进制
- `build/windows/` 等：平台打包资源

音频使用 cgo。Windows、Linux 和 macOS 必须分别在对应系统的原生 runner 上执行 `make build`，不支持从单一系统一次性交叉构建所有平台。

## 检查

```bash
go test ./...
go vet ./...
make test-frontend
make build
```

架构和后续里程碑见 [PLAN.md](PLAN.md)。
