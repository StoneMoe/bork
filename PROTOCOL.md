# Bork Protocol

本文描述当前 `wire-v1`。多字节整数均使用 big-endian。除 STUN/Tracker 外，Bork UDP datagram 最大为 1200 bytes。

## 术语

- `RoomSeed`：邀请内的 256-bit 房间共享秘密。
- `RoomTag`：16-byte packet routing/filter tag。
- `PeerID`：Ed25519 public key 的稳定显示 ID。
- `SenderID`：group datagram header 中的原始 32-byte Ed25519 public key。
- `SessionID`：pairwise X25519 handshake 派生的 16-byte session identifier。
- `StreamID`：sender 为实时 stream 随机生成的 16-byte identifier。
- `Sequence`：从 1 开始、单调递增且不得回绕的 packet/stream sequence。

## Invite

URI：

```text
bork://join/<Base58>
```

Base58 payload：

| Field | Size |
| --- | ---: |
| Version | 1 |
| RoomSeed | 32 |
| UTF-8 room display name | variable, 1–256 bytes / <=64 runes |
| SHA-256 checksum prefix | 4 |

Checksum 覆盖 checksum 之前的完整 payload。Encoding 必须 canonical；当前不读取旧 Base64URL 格式。

## RoomSeed 派生

HKDF-SHA256 salt：

```text
bork/invite/hkdf-sha256/v1
```

| Output | Info | Size |
| --- | --- | ---: |
| TrackerHash | `bork/tracker/v1` | 20 |
| GroupMediaKey | `bork/group-media/v1` | 32 |
| AdmissionKey | `bork/admission/v1` | 32 |
| RoomTag | `bork/room-tag/v1` | 16 |

## Common Prefix

所有 Bork packet 以 22-byte prefix 开始：

| Offset | Field | Size |
| ---: | --- | ---: |
| 0 | Magic `BRK1` | 4 |
| 4 | Version `1` | 1 |
| 5 | PacketType | 1 |
| 6 | RoomTag | 16 |

Packet types：

| Value | Type |
| ---: | --- |
| 1 | Hello |
| 2 | Ping |
| 3 | Pong |
| 4 | BridgeControl |
| 5 | GroupDatagram |
| 6 | Reliable |

未知 type、错误 size、错误 RoomTag、trailing data 或非 canonical field 必须被拒绝。

## Hello

Hello 固定 198 bytes：

| Field | Size | Protection |
| --- | ---: | --- |
| CommonPrefix | 22 | clear |
| Nonce | 16 | signed + admission MAC |
| IdentityKey | 32 | signed + admission MAC |
| Ephemeral X25519 key | 32 | signed + admission MAC |
| HMAC-SHA256 | 32 | `AdmissionKey`, covers prefix + body |
| Ed25519 signature | 64 | covers prefix + body + MAC |

Nonce、identity 和 ephemeral key 不能为零值。相同 Peer identity 的 Hello 被拒绝用于自连接。

## Pairwise Session

双方按 Ed25519 public key lexicographic order 排列 Hello，计算：

```text
TranscriptHash = SHA-256(
  "bork/wire-v1/handshake-transcript\x00" ||
  len(firstHello) || firstHello ||
  len(secondHello) || secondHello
)
```

X25519 shared secret 与 TranscriptHash 经 HKDF-SHA256 派生：

| Output | Info |
| --- | --- |
| SessionID | `bork/wire-v1/session-id` |
| control A->B key | `bork/wire-v1/chacha20poly1305/control/a-to-b` |
| control B->A key | `bork/wire-v1/chacha20poly1305/control/b-to-a` |

## Established Header

Ping、Pong、BridgeControl 和 Reliable 使用 46-byte established header：

| Field | Size |
| --- | ---: |
| CommonPrefix | 22 |
| SessionID | 16 |
| PacketSequence | 8 |

Header 是 ChaCha20-Poly1305 AAD。12-byte nonce 使用 established header 的最后 12 bytes，即 SessionID 后 4 bytes + PacketSequence。每个方向拥有独立 key 和 sequence space；retransmission 必须使用新的外层 PacketSequence。

## Ping / Pong

Ping/Pong encrypted plaintext 为 8-byte random challenge，随后附加 16-byte AEAD tag。总大小固定 70 bytes。

- Ping 发出 challenge。
- Pong 必须回显相同 challenge。
- 只有匹配 pending challenge、session、path 和 replay window 的 Pong 才能认证 path 并更新 RTT。

## BridgeControl

Bridge 只允许一个 intermediary：`Origin -> Forwarder -> Target`。

Outer packet 使用相邻 Peer 的 pairwise control session。Encrypted body：

| Field | Size |
| --- | ---: |
| Origin raw Ed25519 key | 32 |
| Target raw Ed25519 key | 32 |
| Background flag + InnerLength | 1 bit + 15 bits |
| Inner packet | variable, <=1072 |

Origin/Target 必须非零且不同。Inner 只能是同 RoomTag 的 Hello、Ping、Pong 或 Reliable packet。Forwarder：

1. 验证上一跳 session、AEAD、replay window、origin 和 rate budget。
2. Target 必须是 forwarder 的 authenticated direct neighbor。
3. 使用 forwarder-target control session 重新封装相同 inner packet。
4. 不允许递归 bridge。

长度字段最高位表示 background writer lane；只允许 Reliable inner 设置。Forwarder 在重新封装时保留该位，文件数据因此在 direct 和 single-bridge path 上均保持 background 优先级。

## Reliable Message

Reliable 是 pairwise encrypted、message-oriented transport。它不模拟 byte stream。

Encrypted fixed body 为 39 bytes：

| Field | Size |
| --- | ---: |
| Channel | 2 |
| Flags | 1 |
| FragmentSequence | 8 |
| MessageSequence | 8 |
| FragmentIndex | 2 |
| FragmentCount | 2 |
| AckBase | 8 |
| AckBitmap | 8 |
| Payload | variable, <=971 |

Flags：

| Bit | Meaning |
| ---: | --- |
| 0 | Ordered |
| 1 | AckOnly |

规则：

- Channel `0` 保留。
- 一个 channel 的 ordered/unordered mode 由首个 message 固定。
- FragmentCount 范围为 1–1024；应用层继续分块更大的文件。
- AckBitmap bit 0 表示 AckBase，bit N 表示 `AckBase-N`。
- Sender 不发送超过最老 unacked fragment 63 个位置的新 fragment，保证 ACK window 可表示。
- AckOnly packet 的 fragment/message/index/count/payload 必须为零/空。
- Retransmission 保留 logical FragmentSequence，但使用新的外层 PacketSequence 和 AEAD nonce。
- Receiver 只有在成功保留 fragment 后才 ACK；已 ACK reassembly 在 session 生命周期内不会被 TTL/容量 eviction 删除。

当前 transport 参数：

| Parameter | Value |
| --- | ---: |
| Initial congestion window | 4 MSS |
| Minimum congestion window | 2 MSS |
| Maximum congestion window | 64 MSS |
| Initial RTO | 500 ms |
| Minimum / maximum RTO | 100 ms / 5 s |
| Outbound queue safety budget | 4 MiB per Peer |
| Reassembly safety budget | 4 MiB per Peer |

拥塞控制使用 slow start + AIMD；RTT 使用 Karn rule、SRTT 和 RTTVAR。

当前 reliable channels：

| Channel | Payload |
| ---: | --- |
| 1 | Versioned full topology snapshot |
| 2 | Versioned full fan-out assignment |
| 3 | Versioned full member presentation state |
| 4 | Versioned full screen-video state |
| 5 | Versioned full Virtual LAN state |
| 6 | Ordered file control |
| 7 | Unordered file data |

## Topology Message

Reliable channel 1 payload：

| Field | Size |
| --- | ---: |
| Version | 1 |
| Generation | 8 |
| CurrentAudioStreamID | 16 |
| CandidateCount | 2 |
| Candidates | variable |
| NeighborCount | 4 |
| Neighbors | variable |

Address encoding：

| Field | Size |
| --- | ---: |
| Family (`4` / `6`) | 1 |
| Port | 2 |
| IP | 4 / 16 |

Neighbor encoding：raw Peer identity 32 bytes、AddressCount 2 bytes、随后是 addresses。Topology message 是完整版本化 replacement，并由 Reliable 自动分片，因此不受单 datagram entry 数限制。相同 generation 的周期 snapshot 仍会刷新 topology/hint lease；只拒绝更旧 generation。Receiver 仅接受 sender 当前 authenticated session topology 中声明的 Audio StreamID，防止旧 stream 迟到包回滚 decoder。

## Fan-out Assignment

Reliable channel 2 payload：

| Field | Size |
| --- | ---: |
| Version | 1 |
| Generation | 8 |
| ListenerCount | 4 |
| Listener raw Ed25519 keys | `32 * count` |

Assignment 是 speaker 的 SSOT。Forwarder 只接受当前 sender session 上更高 generation 的完整 replacement。空 listener list 撤销旧 assignment。

## Member State

Reliable channel 3 使用 unordered full replacement：

| Field | Size |
| --- | ---: |
| Version | 2 |
| Generation | 8 |
| Flags (`CaptureMuted` bit 0, `PlaybackMuted` bit 1) | 1 |
| UTF-8 Nickname | 0–256 |

Generation 非零并以 authenticated session 为作用域；只接受更高 generation，因此同一 identity 重连后可从 generation 1 重新开始。Nickname 必须是 trim 后的 canonical UTF-8，最多 64 个 Unicode code points、256 bytes，且不包含 control character。

Nickname、CaptureMuted 和 PlaybackMuted 由对应 identity 自声明，只用于 UI 展示。它们通过 pairwise encrypted Reliable 发送，但不代表唯一名称、发言权限或可信 moderation 状态。

## Screen Video

Reliable channel 4 是 31-byte unordered full replacement：Version 1、Generation 8、Active 1、Codec 1、Width 2、Height 2、StreamID 16。Active 时 codec `1` 为 `avc1.42E01F`，`2` 为 `avc1.4D401F`；宽高必须为偶数且不超过 1280x720，StreamID 非零。Inactive 时 codec、尺寸和 StreamID 全为零。

视频使用 Interactive GroupDatagram。每个 payload 以 35-byte fragment header 开始：Version 1、Flags 1、Codec 1、FragmentIndex 2、FragmentCount 2、Generation 8、TimestampMicros 8、DurationMicros 4、TotalChunkSize 4、Width 2、Height 2，随后是 H.264 Annex-B bytes。Flags bit 0 表示关键帧；其他位拒绝。单个 encoded chunk 最大 256 KiB，所有 fragment 元数据必须完全一致；不完整重组 750 ms 后丢弃，丢包后 decoder 等待后续关键帧。

## Virtual LAN

Reliable channel 5 是 30-byte unordered full replacement：Version 1、Enabled 1、Generation 8、StreamID 16、IPv4Address 4。Enabled 时 StreamID 非零且地址是 `100.64.0.0/10` 的非 network/broadcast host；Disabled 时二者全零。地址由 RoomTag 与 identity 确定性派生；重复地址被标记冲突且不路由。

Virtual LAN 使用 CustomRealtime GroupDatagram，Timestamp 为零。Payload 为 Version 1、Target raw Ed25519 key 32、raw IPv4 packet。IPv4 packet 为 20–1000 bytes，version/IHL/total length/header checksum 必须正确；source 必须等于 sender 宣告地址，destination 必须是目标宣告地址或 `100.127.255.255`。普通 unicast 只发送一个 target-specific ciphertext；subnet broadcast 为每个已启用目标分别生成 packet。Forwarder 必须与 sender direct、target 必须与 forwarder direct 且位于 sender 当前 assignment；最多一个 intermediary。

## File Transfer

Channel 6 control message 共同前缀为 Version 1、Kind 1、TransferID 16。Kind 为 Offer、Accept、Reject、Cancel、ACK。Offer 追加 Size 8、SHA-256 32、NameLength 2 和 canonical basename；ACK 追加 next expected offset 8；其他 control 无附加字段。

Channel 7 data message 为 Version 1、TransferID 16、Offset 8、Size 4、Data。Data 为 1–32768 bytes。发送方收到前一 chunk 的精确累计 ACK 后才发送下一 chunk；空文件在 Accept 后直接完成摘要校验。文件最大 1 GiB，session replacement、超时、拒绝或取消会终止传输并清理未完成目标文件。Channel 7 的 Reliable packet 使用 background writer lane。

## GroupDatagram

Group realtime data 使用统一房间 `GroupMediaKey` 和 XChaCha20-Poly1305。

Clear authenticated header：

| Field | Size |
| --- | ---: |
| CommonPrefix | 22 |
| TrafficClass | 1 |
| SenderID | 32 |
| StreamID | 16 |
| Sequence | 8 |

XChaCha20 24-byte nonce：

```text
Nonce = StreamID || Sequence
```

Encrypted body：

| Field | Size |
| --- | ---: |
| Timestamp | 4 |
| Payload | 1–1037 |
| Poly1305 tag | 16 |

Packet 末尾附加 64-byte Ed25519 signature，覆盖 clear header 和完整 ciphertext/tag。

TrafficClass：

| Value | Class |
| ---: | --- |
| 1 | Audio |
| 2 | Interactive |
| 3 | CustomRealtime |

接收顺序：

1. 验证 prefix、RoomTag、class、SenderID、StreamID、sequence 和 packet size。
2. 使用 SenderID public key 验证 Ed25519 signature。
3. 使用统一 GroupMediaKey 和 `StreamID || Sequence` nonce 解密。
4. Commit per-stream replay window 和 source rate budget。
5. 本地消费；若本 Peer 是该 sender 的 assigned forwarder，则转发未修改的原始 packet。

安全语义：

- 所有 RoomSeed holders 可以解密 group data。
- 其他成员不能伪造某个 SenderID，因为没有对应 Ed25519 private key。
- Internet 观察者不能读取 ciphertext。
- 不提供 forward secrecy、成员撤销或 post-compromise history protection。

## Fan-out

- Speaker 依据可靠 topology snapshot 计算 deterministic greedy cover；indirect Virtual LAN target 则固定到其当前 bridge intermediary。
- Forwarder 必须与 speaker direct。
- Listener 必须与 speaker direct，或与其 assigned forwarder direct。
- 最大深度为 `speaker -> forwarder -> listener`。
- Speaker 在当前 forwarder assignments 经 Reliable ACK 后才启用新 plan。
- Forwarder 必须先验证 sender signature、group AEAD、direct source 和 replay window，再发送一个 realtime batch。

## Endpoint Scheduling

Endpoint 单 writer 独占 UDP socket 和 write deadline：

Writer 使用 bounded weighted round-robin cycle：

| Lane | Weight |
| --- | ---: |
| Audio | 8 |
| Control | 2 |
| Interactive | 2 |
| Background | 1 |

空 lane 立即跳过；所有 lane 都为空时才进入 blocking select。持续 audio/control 负载下 interactive 和 background 仍保证取得发送机会。

Audio deadline 上限 20 ms，interactive deadline 上限 50 ms。Realtime queue 满时 drop-oldest whole batch；control admission 满时返回错误，由可靠层或周期性状态发送负责重试。

单次非 audio socket write 的 writer quantum 上限为 2 ms；较大的 interactive batch 在量子耗尽后丢弃剩余 datagram，避免 control、screen、TUN 或 background 写阻塞后续 audio deadline。

单个 realtime batch 最多包含 256 个 datagrams，总 payload bytes 最多为 `256 * 1200`。这是内存与 amplification 安全预算，不是成员数上限；更大的 speaker/forwarder destination set 必须拆成多个 bounded batches。

## Discovery 与公共资源

- Loopback 和 mDNS hints 不经过公共基础设施。
- Tracker 返回值仅是 expiring untrusted hint。
- Tracker endpoint 必须继续通过 signed Hello、RoomSeed admission 和 AEAD Pong。
- 每个 Tracker provider 最多登记四个实际公网 candidates；没有公网 candidate 时才登记 source-observed fallback。
- Tracker 不承载可靠消息、topology、fan-out assignment 或媒体。

## Compatibility

- 当前 wire version 为 `1`。
- Invite version 与 wire version 分别校验。
- 未知 packet type、未知 flag、非 canonical encoding 或 trailing bytes 必须拒绝。
- 当前为首次发布协议；不提供旧 PEX、多跳 relay、pairwise voice 或 Base64URL invite compatibility。
