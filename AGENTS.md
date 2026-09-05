# Bork 开发指南

- 本文档是 Bork 的开发入口；产品要求、工程决策和工作原则分别见 [`docs/PRD.md`](docs/PRD.md)、[`docs/DECISION.md`](docs/DECISION.md) 和 [`docs/WORKSTYLE.md`](docs/WORKSTYLE.md)。
- 当文档与实现冲突时提醒用户。
- 当事实或用户要求有更新时，同步更新相关文档。

## 开发

### 环境要求

- Go 1.26.2 至最新 1.26.x 补丁版本；Wails v2.11.0 的绑定生成器当前不兼容 Go 1.27
- Node.js、npm 和 GNU Make
- Wails v2.11.0 CLI
- 支持 cgo 的原生 C/C++ 工具链

每个目标操作系统都必须使用原生构建环境；音频和 Windows 屏幕共享代码依赖 cgo。

### 仓库结构

| 路径 | 职责 |
| --- | --- |
| `cmd/bork` | 启动 Wails 应用并加载配置 |
| `cmd/bork-driver-helper` | Windows 纯 Go 提权 helper，仅安装和启动 Bork 专用游戏代理驱动 |
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

Bork 将配置存放在用户配置目录下的 `bork/config.yml`。默认构建首次启动时写入以下网络配置：

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

使用 `TAGS=game_proxy` 构建时，首次启动还写入以下游戏代理配置；默认构建不暴露代理配置，但保存网络设置时保留磁盘上已有的 `game_proxy` 数据，避免切换版本丢失节点与凭据：

```yaml
game_proxy:
  directories: []
  node:
    server: ""
    port: 4567
    username: ""
    password: ""
    mtu: 1400
    dns: 1.1.1.1
    encrypt: false
```

各平台配置路径：

- Windows：`%LocalAppData%\bork\config.yml`
- macOS：`~/Library/Application Support/bork/config.yml`
- Linux：`${XDG_CONFIG_HOME:-~/.config}/bork/config.yml`

- 空的 `stun_servers` 列表会禁用公共 STUN 服务。
- 空的 `tracker_urls` 列表会禁用公共 Tracker 服务。
- 将 `port_mapping` 设为 `false` 会禁用网关端口映射。
- 以下游戏代理设置和界面仅在 `TAGS=game_proxy` 构建中提供；`game_proxy.directories` 是 Windows 游戏可执行文件的递归扫描根目录列表，空列表表示尚未配置游戏代理。
- `game_proxy.node.encrypt` 默认关闭；开启后请求服务器使用 iWAN XOR 数据混淆，最终模式以服务器 `OPENACK` 为准。
- 设置中的游戏代理页可导入/导出节点的 Base64 JSON；导出复制已保存节点，导入只填写草稿并保留游戏目录，需另行保存。该内容包含密码，Base64 并非加密，不应公开分享或写入日志。
- 服务器配置默认折叠，点击“配置服务器”展开；保存或导入成功后收起，未保存的导入仍须显式保存。已保存有效节点时，未加速的主页底栏提供“开始加速”；还未添加游戏目录时禁用并提示。加速时底栏提供“停止加速”，并显示连接状态及最近 3 分钟的上行、下行、延迟和探测丢包曲线。速率每秒采样；延迟与丢包仅针对 iWAN 节点 ECHO：每 2 秒探测、2 秒超时，丢包为最近 30 秒已完成探测的超时比例，不是游戏数据包丢失率。
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
- Git 会忽略 helper 构建产物 `internal/gameproxy/netfilter/helper/bork-driver-helper.exe`。
- 品牌源文件位于 `assets/brand/`。
- `make build` 和 `make dev` 会将应用图标复制到 Wails 工作区。
- 通过 `TAGS` 传递额外的 Go build tags。
- 通过 `PLATFORMS` 传递目标平台。
- 不要在 `BUILD_FLAGS` 或 `DEV_FLAGS` 中直接加入 `-tags`。
- 不要在 `BUILD_FLAGS` 中直接加入 `-platform`。

`game_proxy` 是整个游戏代理功能的唯一编译开关。默认构建不包含代理实现、iWAN/gVisor 依赖、
代理 Wails 接口和模型、设置页、底栏或样式，也不需要 NetFilter SDK 或 helper。
完整构建和开发统一使用 Make，通过 `TAGS` 同步选择 Go、Wails 绑定和前端；不要单独设置内部的
`BORK_GAME_PROXY` 环境变量，也不要跳过绑定生成或前端构建，以免切换模式后残留旧产物。

Windows 游戏代理使用 `make build TAGS=game_proxy` 或 `make dev TAGS=game_proxy`；
两者及 `make typecheck-frontend TAGS=game_proxy` 都会先执行 `prepare-netfilter-helper`，依赖 `verify-netfilter-sdk` 校验后，以
`CGO_ENABLED=0 GOOS=windows GOARCH=amd64` 和 `-tags game_proxy -trimpath -ldflags '-s -w -H=windowsgui'`
单独编译 `cmd/bork-driver-helper` 到上述忽略路径，再由 GUI 内嵌。直接运行带 tag 的测试或 vet 前，
先执行 `make prepare-netfilter-helper`，再运行 `go test -tags game_proxy ./...` 和 `go vet -tags game_proxy ./...`。
SDK 获取与校验见 [`internal/gameproxy/netfilter/SDK.md`](internal/gameproxy/netfilter/SDK.md)。
GitHub Actions 的 `Windows NetFilter Demo` 工作流会在 push、PR 或手动触发时构建和测试，
先验证没有 SDK/helper 的默认版本与绑定/前端隔离，再获取 SDK、准备 helper 并验证启用版本，成功后通过 `actions/upload-artifact@v7` 的 `archive: false`
直接上传 `build/bin/bork.exe`，artifact 名为 `bork.exe`，保留 14 天。构建、测试和 CI
都不得运行 helper 或安装、启动驱动；通过 CI 不代表真实 WFP 拦截已经验收。

Windows 开发、测试和正式版本统一交付单个 `bork.exe`，无配套 ZIP 或 MSIX；Microsoft Store
当前不是分发目标。单文件仅指交付，启用游戏代理的版本运行时仍会释放内嵌的驱动、DLL 和 helper；其他平台使用各自原生格式，
不要求 macOS/Linux 使用 `.exe`。当前未签名的游戏代理贡献者 EXE 内嵌原始签名 demo 驱动，
不修改驱动签名或绕过签名检查，也不授予生产 SDK 许可。设置的游戏代理页通过
`GetGameProxyLicense()` 获取内嵌原始 RTF，仅在用户点击导出时通过浏览器 Blob 保存，不生成分发 sidecar。

游戏代理 Start 先以普通权限探测 `bork_netfilter_demo` 服务；缺失或已停止且通过 Bork 驱动校验时，
由独立 helper 请求 UAC 授权后安装、启动，已运行则直接使用。主界面保持普通用户权限，
不重启或主动断开语音；不保证 UAC 安全桌面期间全局按键说话可用。不能把通用初始化失败视为提权理由。
取消授权不退出主应用，迟到的授权不得恢复已取消的代理；安装开始后，取消仍可能留下已安装、已启动的驱动，
普通 Stop 不停止或卸载系统驱动。helper 不读取用户节点配置或凭据，只使用受保护的
`%WINDIR%\System32\drivers\bork_netfilter_demo.sys` 和 `%WINDIR%\System32\bork-netfilter-demo`
驱动/DLL 路径。不接管、迁移或删除旧的手工 `netfilter2` 安装。下载、授权和安全手动卸载说明见
[`docs/windows-netfilter-demo.md`](docs/windows-netfilter-demo.md)。

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
