<p align="center">
  <img src="assets/brand/appicon.png" alt="Bork" width="240">
</p>

Bork 是一个低资源占用、低延迟、去中心化的游戏语音软件。

## 功能

- 低延迟多人语音，无需注册账号，无需服务器，无需互联网
- 支持屏幕共享和文件传输
- 支持自定义全局按键说话（Linux 按键由桌面系统确认）
- 支持回声消除、神经网络降噪与多人响度均衡
- 支持 Windows、macOS 和 Linux

## 贡献者测试版

游戏代理是可选编译功能：默认 `make build` / `make dev` 不包含代理后端或界面；
开发游戏代理版本使用 `make build TAGS=game_proxy` / `make dev TAGS=game_proxy`，
环境和 SDK 准备见[开发指南](AGENTS.md)。切换到默认版不会删除已有节点配置。

Windows 游戏代理开发版可从 GitHub Actions 的 **Windows NetFilter Demo** 成功运行页面下载，
直接下载单个 `bork.exe`，保留 14 天，无需解压 ZIP 或下载配套文件。当前 EXE 未签名，
内嵌的 NetFilter demo 驱动保留发布者原始签名，不绕过 Windows 驱动签名检查。

首次启动游戏代理时，若 Bork 专用驱动尚未安装或未运行，独立 helper 会请求 UAC 授权并安装、启动驱动；
主界面保持普通用户权限，不为安装驱动而主动断开语音。取消授权不会退出 Bork，停止代理不卸载驱动。
原始 SDK 许可可在设置的游戏代理页显式导出到本机。

Windows 开发、测试和正式版本均使用单 EXE 交付，不提供配套 ZIP 或 MSIX；Microsoft Store 当前不是分发目标。
单文件分发不代表运行时不释放驱动、DLL 和 helper，也不授予正式 SDK 许可：当前 demo 仅获准用于贡献者测试，
不是正式发行版。其他平台保留各自原生格式，并非要求 macOS/Linux 使用 `.exe`。
使用前请阅读[下载与驱动测试指南](docs/windows-netfilter-demo.md)；CI 不运行 helper、不安装或启动驱动，也不验证真实拦截行为。
