# 当前的工程决策

## 游戏代理编译隔离

- 整个游戏代理仅由 `game_proxy` build tag 启用；无标签时不编译 `internal/gameproxy`、iWAN/gVisor、驱动 helper、代理 Wails 方法/模型或代理前端。非 Windows/amd64/cgo 的启用版本使用不支持原生拦截的 factory，而非默认版本携带该 stub。
- Make 的 `TAGS` 是完整构建的单一入口，同步传给 Go/Wails，并导出内部 `BORK_GAME_PROXY` 供 Vite 和 TypeScript 选择同一前端模块。默认模块不导入代理组件、绑定或 CSS；切换模式重新生成绑定和前端，禁止跳过这些步骤。
- `AppSnapshot` 和 `AppConfig` 各保留两份小型平铺定义，避免 Wails 跳过匿名字段或生成无标签代理模型。公共 App 生命周期只保留私有 hooks，无标签实现为空操作。
- 默认配置只包含网络设置；读取旧文件时不暴露其中的代理字段，保存时保留最新磁盘上的 `game_proxy` 数据及凭据。启用版本继续校验 typed 代理配置，保存代理设置时保留最新网络设置。

## 游戏目录扫描

- 扫描先按目录项类型和 `.exe` 扩展名筛选，避免为无关资源文件额外查询 Windows 属性；保留目录重解析点跳过、普通文件校验和每个根目录的规范路径边界检查。
- 多个根目录共用去重集合，最后统一排序；不按字符串前缀删除嵌套根目录，因为显式配置的目录可能位于另一个根目录跳过的 junction 下。不引入扫描缓存或后台索引。
- 启动扫描使用本次运行的 context，目录更新扫描使用调用方 context；逐项检查取消，取消不返回部分规则。取消是协作式的，不能中断正在执行的文件系统调用、单个目录的读取或排序。

## Windows 交付与驱动

- Windows 开发、测试和正式版本统一交付单个 `bork.exe`，不分发配套 ZIP 或 MSIX；Microsoft Store 当前不是目标。其他平台继续采用各自原生格式，不把 `.exe` 要求扩展到 macOS/Linux。保留未使用的品牌和 manifest 源资产。
- Windows 游戏代理 GUI 内嵌原始签名 NetFilter demo 驱动、API DLL、原始 RTF 许可和独立 helper；运行时释放系统所需文件不是分发 sidecar。当前 EXE 未签名，不修改驱动签名、不绕过 Windows 签名检查；已批准的贡献者 demo 分发不包含生产 SDK 授权。
- `cmd/bork-driver-helper` 单独以 `CGO_ENABLED=0 GOOS=windows GOARCH=amd64` 和 `-tags game_proxy -trimpath -ldflags '-s -w -H=windowsgui'` 构建到忽略的 `internal/gameproxy/netfilter/helper/bork-driver-helper.exe`，再由 GUI 内嵌。`prepare-netfilter-helper` 依赖 `verify-netfilter-sdk`；`build`、`dev` 和 `bindings` 仅在 `TAGS` 包含 `game_proxy` 时依赖它，Wails 的 tag 继续仅通过 `TAGS` 传入。
- CI 先验证不含 SDK/helper 的默认构建及依赖、绑定和前端隔离，再获取 SDK、编译 helper 并验证启用版本；最终用 `actions/upload-artifact@v7` 的 `archive: false` 直接上传启用游戏代理的 `build/bin/bork.exe`，不执行 staging。构建和测试不运行 helper、不安装或启动驱动；原生验收必须另行显式进行，不能由 CI 结果推断。
- Start 先以普通权限探测专用服务，仅在缺失或已停止且校验为 Bork 驱动时由独立 helper 请求 UAC 安装、启动；已经运行则直接使用。主界面声明 `asInvoker`，正常启动时保持普通用户权限，不重启、不主动断开语音；通用 `nf_init` 错误不作为提权判据，不保证安全桌面上的全局按键说话。
- helper 不读取用户节点配置或凭据，只安装、启动 `bork_netfilter_demo`，驱动路径固定为 `%WINDIR%\System32\drivers\bork_netfilter_demo.sys`，DLL 目录为受保护的 `%WINDIR%\System32\bork-netfilter-demo`。名称变化不修改原始签名字节；同名但身份或路径不符的服务/文件不得覆盖。不接管、迁移或删除手工 `netfilter2` 安装。
- 取消 UAC 不退出主应用，迟到的授权不得恢复已取消的代理启动；系统安装一旦开始便不承诺随取消回滚。普通 Stop 仅停止代理，不停止或卸载驱动；卸载只按文档由管理员校验专用服务、停止并删除服务后清理已知 Bork 文件，不递归清除未知文件或自动重启。
- `GetGameProxyLicense()` 返回内嵌原始 SDK RTF；设置中的游戏代理页仅在用户点击导出时通过浏览器 Blob 保存到本机，不在启动或分发时自动写入许可 sidecar。

## 界面语言

- 前端统一决定有效语言：语言设置默认为 `auto`，所有 `zh` 语言变体映射到 `zh-CN`，其余映射到 `en`。手选 `zh-CN` 或 `en` 后立即更新界面，并写入 WebView 的 `localStorage` 键 `bork.language`；选择自动时删除该键。
- `GetSystemLanguage` 读取当前用户的界面首选语言，不以日期和数字的区域格式代替。Windows 使用现有 `x/sys/windows` 的用户 UI 语言接口，macOS 使用 `CFLocaleCopyPreferredLanguages`，Linux 使用消息语言环境变量；原生接口不可用时使用 WebView 的 `navigator.language`。
- 首次渲染前读取系统语言。自动模式在窗口重新获得焦点或收到 `languagechange` 时重新读取，手选语言不随系统变化。
- 中文源文案同时作为翻译键，英文文案集中在前端词典，通过现有 Solid 响应式状态更新，无新增国际化依赖。原生文件选择与保存对话框的标题由前端翻译后传入，Go 不另存语言偏好或维护翻译词典。

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
- 创建 Session 时立即发送本地 Hello；主动探测新路径、响应 Hello probe 或纠正旧 Session Hello 时也可发送当前 Hello。发送入口统一登记候选路径，避免依赖对方再回一个 Hello 才能接收该路径上的 Ping/Pong；候选路径仍须通过 Ping/Pong 验证。
- 收到与已有 Session 匹配的 Hello 时，只补齐密钥、登记候选路径并发送 Ping，不再回复 Hello。尚未完成认证的 pending Session 仅由现有 2 秒探测定时器向原路径和已有候选路径重发 Hello；不维护每个 Session 的 Hello 发送时钟。首次发送临近定时器时允许很快重复一次。
- Session 经有效 Pong 完成认证后停止 Hello 重发，继续用 Ping/Pong 测量延迟和检查连通性。报文格式保持不变；建议双方同时更新，旧版在旧 Hello 纠正分支遗漏候选路径登记时，混用版本的路径恢复仍可能等待重新发现或超时重建。
- Room Datagram 使用房间共享密钥执行 AEAD。Voice StreamID 等于 PeerID；每次开始或替换屏幕分享时生成新的 Screen StreamID。可信 Forwarder 验证并转发原始数据包，不重新编码或加密。
- 群组数据在互联网上保持加密，互联网观察者无法读取媒体。
- 每个 Session 使用临时 X25519 密钥派生点对点控制加密密钥。
- 对于桥接流量，Bridge 会解密其相邻 Session 的外层数据包。
- Hello probe 和 Session Hello 内层数据包未加密，因此 Bridge 可以读取。
- Bridge 无法解密端到端内层 Ping、Pong 或 Reliable 载荷。
- Room Datagram 不具备前向保密、成员撤销或旧密文的入侵后保护。不得将当前 `RoomDatagramKey` 描述为具备前向保密的群组 E2EE。
