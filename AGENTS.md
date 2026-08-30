# Bork 开发指南

- 本文档是 Bork 的开发入口；产品要求、工程决策和工作原则分别见 [`docs/PRD.md`](docs/PRD.md)、[`docs/DECISION.md`](docs/DECISION.md) 和 [`docs/WORKSTYLE.md`](docs/WORKSTYLE.md)。
- 当文档与实现冲突时提醒用户。
- 当事实或用户要求有更新时，同步更新相关文档。

## 开发

### 环境要求

- Go 1.26.2 或更高版本
- Node.js、npm 和 GNU Make
- Wails v2 CLI
- 支持 cgo 的原生 C/C++ 工具链

每个目标操作系统都必须使用原生构建环境；音频和 Windows 屏幕共享代码依赖 cgo。

### 仓库结构

| 路径 | 职责 |
| --- | --- |
| `cmd/bork` | 启动 Wails 应用并加载配置 |
| `internal/app` | 管理生命周期、命令和 UI 快照 |
| `internal/audio` | 采集、处理、编码、混音和播放音频 |
| `internal/screenshare` | 枚举、采集、色调映射并编码本机屏幕与系统声音 |
| `internal/networking` | 管理发现、单一房间 UDP 端点、端口映射和房间网络 |
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
- 默认 Tracker 可看到派生的 tracker hash、派生的 20 字节 tracker `peer_id`、候选地址和源地址，但无法获取 `RoomSeed`、房间状态或媒体明文；tracker `peer_id` 由本次入房的 `PeerID` 派生。

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

### 名词表

| 名词 | 含义 |
| --- | --- |
| `PeerID` | 一次入房期间随机生成的 16 字节成员标识，也是 Go 内部表示成员身份的唯一类型；字段可按上下文叫 `peerId`、`origin` 或 `target`，但 Go 类型统一为 `identity.PeerID`。 |
| `SessionID` | 一次点对点 Session 的 16 字节随机标识，由 PeerID 较小的一方生成，并绑定到双方的 Session Hello transcript。 |
| `StreamID` | 一路 Room Datagram 媒体流的标识。 |
| `PacketSequence` | Session 包或 Room Datagram 的包级序号，用于 nonce 和重放检查。 |
| `FragmentSequence` | Reliable 分片的确认序号。 |
| `MediaUnitID` | 媒体载荷自己的标识；语音使用采样位置，屏幕视频使用 chunk 序号。 |
| `Revision` | 仅在本机判断最新全量状态是否已排队，不在线上传输；发送队列失效标记单独叫 `SendGeneration`。 |
