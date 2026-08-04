# Bork TODO

## P0：真实设备与网络验收

- [ ] 两台原生设备关闭 STUN，在普通 IPv4 LAN 内自动认证 <=10 s
- [ ] 有线耳机进行 30 分钟双向通话与 mute/unmute 恢复
- [ ] 每方向至少记录 100 个 mouth-to-ear samples
- [ ] 验证 LAN mouth-to-ear p95 <=60 ms
- [ ] 家庭宽带、手机热点、CGNAT、IPv6 记录 direct/bridge/fan-out matrix
- [ ] 验证 PCP、NAT-PMP、UPnP mapping 建立、续租、过期清理和退出删除
- [ ] 破坏 bridge/forwarder 链路，验证 topology 更新和 assignment 重建

## P1：规模与性能

- [ ] 20、50、100 个模拟 Peer 的 topology/reliable/fan-out soak test
- [ ] 20–50 台真实设备或等价网络节点的持续 room test
- [ ] 多 speaker 同时发送时记录 CPU、内存、pps、queue drops 和 verify cost
- [ ] Benchmark Ed25519 verify 在目标平台上的 100、1000、5000 packets/s 成本
- [ ] 验证无 count-based member/speaker rejection
- [ ] 验证 realtime overload 只产生 bounded frame loss，不产生持续 latency backlog
- [ ] 验证 forwarder upload 在 fan-out plan 下分布合理
- [ ] 验证 reliable ACK-window、loss recovery 和 4 MiB budgets 的长时间稳定性

## P2：自适应媒体

- [ ] 从 endpoint queue pressure、RTT 和 packet loss 生成 media feedback
- [ ] Audio bitrate 从“按房间人数”升级为“人数 + loss + queue pressure”
- [ ] 为 interactive stream 定义 bitrate、fragmentation、keyframe 和 FEC 策略
- [ ] 验证 bitrate 调整不阻塞 10 ms audio hot path
- [ ] 评估 Ed25519 batch/hash-chain 优化；只能在不降低 sender authenticity 时采用

## P3：房间状态与文本

- [ ] 定义 presence lease 和过期规则
- [ ] 定义分区后的 room-state merge 语义
- [ ] 在 Reliable channel 上实现 text message
- [ ] 实现 history pagination 和 bounded retention
- [ ] UI 展示连接、bridge、fan-out 和 reliable diagnostics

## P4：文件传输

- [ ] 定义 background reliable channel
- [ ] 应用层 bounded chunks、hash、resume 和 cancel
- [ ] 全局/per-peer bandwidth budget
- [ ] 验证文件传输不会增加 audio p95 latency
- [ ] 处理磁盘空间、路径和恶意文件名

## P5：Camera 与 Screen

- [ ] 定义 interactive stream signaling
- [ ] 定义 codec/profile negotiation
- [ ] 实现 datagram fragmentation、loss feedback、FEC 和 keyframe request
- [ ] 复用 group encryption、sender signature 和 fan-out assignment
- [ ] 验证 video/screen overload 不影响 audio lane

## P6：Audio 质量

- [ ] AEC
- [ ] Noise suppression
- [ ] Automatic gain control
- [ ] Audio device hot-plug
- [ ] Sleep/wake recovery
- [ ] RTP interoperability 评估
- [ ] FEC/packet-loss 参数实测调优
- [ ] 在真实麦克风、键盘噪声和回声环境下校准 VAD threshold/release

## P7：安全与硬化

- [ ] 扩展 wire、invite、Tracker、portmap parser fuzzing
- [ ] 重组内存、queue、signature verify 和 forwarding amplification 攻击测试
- [ ] RoomSeed 泄露和恶意 room member 的威胁模型文档
- [ ] 验证 sender signature 在所有 group feature 中不可绕过
- [ ] 丢包、乱序、重复、延迟、分区和 bridge churn fault injection
- [ ] Windows、macOS、Linux 原生 build/release 验证

## 验收指标

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
