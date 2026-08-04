import {
  For,
  Show,
  createMemo,
  createSignal,
  onCleanup,
} from "solid-js";
import * as Backend from "@wailsjs/go/app/App";
import { ClipboardSetText } from "@wailsjs/runtime/runtime";
import { createRemoteState } from "./sync";
import type { AppState, Candidate, FriendlyStatus, RemotePeer } from "./types";

const maxInviteLength = 512;

function humanStatus(state: AppState): FriendlyStatus {
  if (!state.peerId && state.error) return { badge: "启动失败", title: "", detail: "" };
  if (!state.peerId) return { badge: "正在启动", title: "", detail: "" };
  if (!state.room) return { badge: "准备就绪", title: "", detail: "" };
  if (state.room.phase === "gathering") {
    return {
      badge: "正在准备",
      title: "正在准备连接",
      detail: "Bork 正在打开通信端口并检查网络环境。",
    };
  }
  const remotePeers = state.room.remotePeers;
  if (remotePeers.length > 0) {
    return {
      badge: state.audio.running ? (state.audio.muted ? "通话中 · 已静音" : "语音通话中") : `已连接 ${remotePeers.length}`,
    };
  }
  return {
    badge: "正在寻找",
    title: "正在寻找房间成员",
    detail: "保持 Bork 运行，其他成员上线后会自动尝试连接。",
  };
}

export default function App() {
  const [busy, setBusy] = createSignal(false);
  const [settingsOpen, setSettingsOpen] = createSignal(false);
  const [error, setError] = createSignal("");
  const remote = createRemoteState(setError);
  const state = remote.state;
  const operational = createMemo(() => Boolean(state().peerId));
  const inRoom = createMemo(() => Boolean(state().room));
  const friendly = createMemo(() => humanStatus(state()));

  async function runAction(action: () => Promise<void>) {
    if (!operational() || busy()) return false;
    setBusy(true);
    setError("");
    try {
      await action();
      await remote.refresh();
      return true;
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : String(cause || "未知错误");
      setError(message.replace(/^Error:\s*/, ""));
      return false;
    } finally {
      setBusy(false);
    }
  }

  return (
    <main class="shell">
      <header class="topbar">
        <div class="wordmark">
          BORK<span>/</span>
          <span classList={{ "room-name": inRoom() }}>
            {state().room?.name || "VOICE"}
          </span>
        </div>
        <div class="topbar-actions">
          <div
            class="status-pill"
            classList={{ active: inRoom() }}
          >
            <i />
            <span>{friendly().badge}</span>
          </div>
          <button class="settings-button" type="button" disabled={!operational()} onClick={() => setSettingsOpen(true)}>
            设置
          </button>
        </div>
      </header>

      <section class="main-view">
        <Show
          when={inRoom()}
          fallback={<Lobby busy={busy()} ready={operational()} runAction={runAction} />}
        >
          <Room
            state={state()}
            friendly={friendly()}
            busy={busy()}
            ready={operational()}
            runAction={runAction}
          />
        </Show>
      </section>

      <Show when={settingsOpen()}>
        <Settings
          state={state()}
          busy={busy()}
          ready={operational()}
          close={() => setSettingsOpen(false)}
          runAction={runAction}
        />
      </Show>

      <Show when={error()}>
        <div class="error" role="alert">
          <strong>出现问题</strong>
          <span>{error()}</span>
          <button type="button" aria-label="关闭" onClick={() => setError("")}>关闭</button>
        </div>
      </Show>
    </main>
  );
}

interface ActionProps {
  busy: boolean;
  ready: boolean;
  runAction: (action: () => Promise<void>) => Promise<boolean>;
}

function Lobby(props: ActionProps) {
  const [roomName, setRoomName] = createSignal("");
  const [invite, setInvite] = createSignal("");

  async function createRoom(event: SubmitEvent) {
    event.preventDefault();
    if (await props.runAction(() => Backend.CreateRoom(roomName().trim()))) setRoomName("");
  }

  async function joinRoom(event: SubmitEvent) {
    event.preventDefault();
    if (await props.runAction(() => Backend.JoinRoom(invite().trim()))) setInvite("");
  }

  return (
    <section class="lobby-view">
      <div class="lobby-heading">
        <p class="eyebrow">VOICE ROOMS / DISTRIBUTED</p>
        <h1>世界末日<br />照样通信</h1>
        <p>不用账号，也不用服务器，甚至没有互联网也能立刻开始通信</p>
      </div>
      <div class="invite-stack">
        <form class="room-form" onSubmit={createRoom}>
          <label for="roomName">创建房间</label>
          <input
            id="roomName"
            autocomplete="off"
            placeholder="给房间起个名字"
            value={roomName()}
            onInput={(event) => setRoomName(event.currentTarget.value)}
            required
          />
          <button class="button primary" type="submit" disabled={props.busy || !props.ready}>创建并进入</button>
        </form>
        <div class="form-divider"><span>或者</span></div>
        <form class="room-form compact" onSubmit={joinRoom}>
          <label for="inviteInput">加入房间</label>
          <textarea
            id="inviteInput"
            maxlength={maxInviteLength}
            spellcheck={false}
            autocomplete="off"
            placeholder="粘贴房间邀请"
            value={invite()}
            onInput={(event) => setInvite(event.currentTarget.value)}
            required
          />
          <button class="button quiet" type="submit" disabled={props.busy || !props.ready}>加入</button>
        </form>
      </div>
    </section>
  );
}

interface RoomProps extends ActionProps {
  state: AppState;
  friendly: FriendlyStatus;
}

function Room(props: RoomProps) {
  const [copied, setCopied] = createSignal(false);
  let copyTimer: number | undefined;
  onCleanup(() => {
    window.clearTimeout(copyTimer);
  });

  async function copyInvite() {
    const copiedInvite = await props.runAction(async () => {
      const invite = await Backend.GetInvite();
      if (!invite) throw new Error("当前没有房间邀请");
      if (!await ClipboardSetText(invite)) throw new Error("无法写入系统剪贴板");
    });
    if (!copiedInvite) return;
    setCopied(true);
    window.clearTimeout(copyTimer);
    copyTimer = window.setTimeout(() => {
      setCopied(false);
    }, 1600);
  }

  const remotePeers = () => props.state.room?.remotePeers ?? [];

  return (
    <section class="room-view">
      <div class="room-content">
        <section class="voice-stage">
          <div class="room-actions">
            <button class="invite-button" type="button" disabled={props.busy || !props.ready} onClick={copyInvite}>
              {copied() ? "已复制" : "复制邀请"}
            </button>
            <button
              class="invite-button leave-button"
              type="button"
              disabled={props.busy || !props.ready}
              onClick={() => props.runAction(Backend.LeaveRoom)}
            >离开房间</button>
          </div>
          <Show
            when={remotePeers().length > 0 || props.state.audio.running}
            fallback={
              <div class="voice-caption waiting-state">
                <strong>{props.friendly.title}</strong>
                <p>{props.friendly.detail}</p>
              </div>
            }
          >
            <section class="room-peers" aria-label="房间成员">
              <RoomMemberList state={props.state} remotePeers={remotePeers()} />
              <Show when={props.state.audio.running}>
                <div class="voice-controls">
                  <button
                    class="button quiet"
                    type="button"
                    disabled={props.busy || !props.ready}
                    onClick={() => props.runAction(() => Backend.SetMuted(!props.state.audio.muted))}
                  >{props.state.audio.muted ? "取消静音" : "静音"}</button>
                </div>
              </Show>
              <Show when={props.state.audio.error}>
                <p class="voice-error">{props.state.audio.error}</p>
              </Show>
              <Show when={!props.state.audio.available && !props.state.audio.error}>
                <p class="voice-error">没有可用的麦克风或扬声器。</p>
              </Show>
            </section>
          </Show>
        </section>
      </div>
    </section>
  );
}

function RoomMemberList(props: { state: AppState; remotePeers: RemotePeer[] }) {
  const localSpeaking = () => props.state.audio.speaking && !props.state.audio.muted;
  const remoteSpeaking = (remotePeer: RemotePeer) => !remotePeer.muted && props.state.audio.speakingPeerIds.includes(remotePeer.peerId);
  const remoteName = (remotePeer: RemotePeer) => remotePeer.nickname || remotePeer.peerId.slice(0, 14);
  const localStatus = () => props.state.audio.muted ? "已静音" : localSpeaking() ? "正在说话" : "空闲";
  const remoteStatus = (remotePeer: RemotePeer) => remotePeer.muted ? "已静音" : remoteSpeaking(remotePeer) ? "正在说话" : "空闲";
  const remoteTransport = (remotePeer: RemotePeer) => remotePeer.transport === "bridge" ? "桥接" : "直连";
  return (
    <div class="member-list" aria-label="当前房间成员">
      <div
        class="member-row local-member"
        classList={{ speaking: localSpeaking() }}
        tabindex="0"
        aria-label={`${props.state.nickname || "本机"}，本机，${localStatus()}`}
      >
        <strong class="member-name">{props.state.nickname || "本机"}</strong>
        <span class="member-connection local">本机</span>
        <span class="member-latency">—</span>
        <span class="member-status">{localStatus()}</span>
        <div class="member-details">
          <span><small>PeerID</small><code>{props.state.peerId || "正在载入"}</code></span>
          <span><small>本机端点</small><code>{props.state.diagnostics.listenAddress || "尚未打开"}</code></span>
          <span><small>房间状态</small><b>{props.state.room?.phase || "未知"}</b></span>
        </div>
      </div>
      <For each={props.remotePeers}>{(remotePeer) => (
        <div
          class="member-row"
          classList={{ speaking: remoteSpeaking(remotePeer) }}
          tabindex="0"
          aria-label={`${remoteName(remotePeer)}，${remoteTransport(remotePeer)}，${remotePeer.rttMillis || 1} 毫秒，${remoteStatus(remotePeer)}`}
        >
          <strong class="member-name">{remoteName(remotePeer)}</strong>
          <span class="member-connection" classList={{ bridge: remotePeer.transport === "bridge" }}>
            {remoteTransport(remotePeer)}
          </span>
          <span class="member-latency">{remotePeer.rttMillis || 1} ms</span>
          <span class="member-status">{remoteStatus(remotePeer)}</span>
          <div class="member-details">
            <span><small>PeerID</small><code>{remotePeer.peerId}</code></span>
            <span><small>Session</small><code>{remotePeer.sessionId || "未知"}</code></span>
            <span>
              <small>{remotePeer.transport === "bridge" ? "下一跳" : "远端地址"}</small>
              <code>{remotePeer.address}</code>
            </span>
          </div>
        </div>
      )}</For>
    </div>
  );
}

interface SettingsProps extends ActionProps {
  state: AppState;
  close: () => void;
}

function Settings(props: SettingsProps) {
  const audio = () => props.state.audio;
  const diagnostics = () => props.state.diagnostics;
  const candidates = () => diagnostics().candidates || [];
  const stun = () => diagnostics().stun || [];
  const trackers = () => diagnostics().tracker || [];
  const connectivity = () => diagnostics().connectivity;
  const knownAddresses = () => connectivity()?.knownAddresses || [];
  const diagnosticErrors = () => [diagnostics().networkError, diagnostics().discoveryError, diagnostics().portMappingError]
    .filter((message): message is string => Boolean(message));
  const [nickname, setNickname] = createSignal(props.state.nickname);
  const [now, setNow] = createSignal(Date.now());
  const clock = window.setInterval(() => setNow(Date.now()), 1000);
  onCleanup(() => window.clearInterval(clock));

  async function saveNickname(event: SubmitEvent) {
    event.preventDefault();
    if (await props.runAction(() => Backend.SetNickname(nickname()))) setNickname(props.state.nickname);
  }

  return (
    <div class="settings-layer">
      <button class="settings-backdrop" type="button" aria-label="关闭设置" onClick={props.close} />
      <aside class="settings-drawer" aria-label="设置">
        <header class="settings-header">
          <div><span>SETTINGS</span><strong>设置</strong></div>
          <button type="button" aria-label="关闭设置" onClick={props.close}>关闭</button>
        </header>
        <section class="settings-section">
          <div class="settings-section-heading">
            <h3>语音设备</h3>
            <button
              class="section-action"
              type="button"
              disabled={props.busy || !props.ready || props.state.audio.running}
              onClick={() => props.runAction(Backend.RefreshAudioDevices)}
            >刷新</button>
          </div>
          <label class="audio-device-field">
            <span>麦克风</span>
            <select
              value={audio().captureDeviceId}
              disabled={props.busy || !props.ready || props.state.audio.running}
              onChange={(event) => props.runAction(() => Backend.SetAudioDevices(event.currentTarget.value, audio().playbackDeviceId))}
            >
              <option value="">系统默认</option>
              <For each={audio().captureDevices}>{(device) => (
                <option value={device.id}>{device.name}{device.isDefault ? "（默认）" : ""}</option>
              )}</For>
            </select>
          </label>
          <label class="audio-device-field">
            <span>扬声器</span>
            <select
              value={audio().playbackDeviceId}
              disabled={props.busy || !props.ready || props.state.audio.running}
              onChange={(event) => props.runAction(() => Backend.SetAudioDevices(audio().captureDeviceId, event.currentTarget.value))}
            >
              <option value="">系统默认</option>
              <For each={audio().playbackDevices}>{(device) => (
                <option value={device.id}>{device.name}{device.isDefault ? "（默认）" : ""}</option>
              )}</For>
            </select>
          </label>
          <Show when={props.state.audio.error}>
            <p class="diagnostic-error">{props.state.audio.error}</p>
          </Show>
          <Show when={!props.state.audio.available && !props.state.audio.error}>
            <p class="empty-diagnostic">没有可用的麦克风或扬声器。</p>
          </Show>
        </section>
        <section class="settings-section">
          <h3>设备</h3>
          <form class="nickname-form" onSubmit={saveNickname}>
            <label for="nickname">房间昵称</label>
            <div>
              <input
                id="nickname"
                autocomplete="nickname"
                placeholder="未设置"
                value={nickname()}
                disabled={props.busy || !props.ready}
                onInput={(event) => setNickname(event.currentTarget.value)}
              />
              <button type="submit" disabled={props.busy || !props.ready}>保存</button>
            </div>
            <small>最多 64 个字符，加入房间后对其他成员可见。</small>
          </form>
          <div class="setting-row stacked">
            <span>用户身份</span>
            <code>{props.state.peerId || "正在载入"}</code>
          </div>
        </section>
        <section class="settings-section">
          <h3>连接诊断</h3>
          <div class="setting-row stacked">
            <span>本机端点</span>
            <Show when={diagnostics().listenAddress}>
              <code>{`${diagnostics().listenAddress}（UDP）`}</code>
            </Show>
            <Show when={!diagnostics().listenAddress}>
              <small>{props.state.room ? "正在打开本机 UDP 端点。" : "加入房间后打开 UDP 端点。"}</small>
            </Show>
          </div>
          <div class="diagnostic-heading"><span>本机候选地址</span><b>{candidates().length}</b></div>
          <ol class="candidate-list">
            <For each={candidates()}>{(candidate) => <CandidateRow candidate={candidate} />}</For>
          </ol>
          <Show when={candidates().length === 0}>
            <div class="empty-diagnostic">{props.state.room ? "尚未发现可用的本机候选地址。" : "加入房间后开始收集本机候选地址。"}</div>
          </Show>
          <div class="diagnostic-heading"><span>STUN 探测</span></div>
          <ol class="stun-list">
            <For each={stun()}>{(result) => (
              <li classList={{ failed: !result.mappedAddress }} title={result.error || ""}>
                <span>{result.server}</span>
                <b>{result.mappedAddress ? `${result.rttMillis || 1} ms` : "失败"}</b>
              </li>
            )}</For>
          </ol>
          <Show when={stun().length === 0}>
            <div class="empty-diagnostic">{props.state.room ? "尚未获得 STUN 探测结果。" : "加入房间后开始 STUN 探测。"}</div>
          </Show>
          <div class="diagnostic-heading"><span>Tracker 公告</span></div>
          <ol class="stun-list">
            <For each={trackers()}>{(tracker) => (
              <li classList={{ failed: Boolean(tracker.error) }} title={tracker.error || `返回 ${tracker.peerCount} 个地址`}>
                <span><strong>{tracker.provider}</strong><small>请求 {tracker.candidate}</small></span>
                <span class="tracker-result">
                  <b>{tracker.error ? "失败" : tracker.observedAddress || "未返回"}</b>
                  <small>{tracker.nextAnnounce ? formatRelativeTime(tracker.nextAnnounce, now()) : "等待 announce"}</small>
                </span>
              </li>
            )}</For>
          </ol>
          <Show when={trackers().length === 0}>
            <div class="empty-diagnostic">{props.state.room ? "尚未产生 Tracker announce 记录。" : "加入房间后开始 Tracker announce。"}</div>
          </Show>
          <div class="diagnostic-heading"><span>已知地址</span><b>{knownAddresses().length}</b></div>
          <ol class="candidate-list known-address-list">
            <For each={knownAddresses()}>{(address) => (
              <li>
                <b>{address.source}</b>
                <div>
                  <code>{address.address}</code>
                  <small>{address.expiresAt}</small>
                </div>
              </li>
            )}</For>
          </ol>
          <Show when={knownAddresses().length === 0}>
            <div class="empty-diagnostic">{props.state.room ? "尚未发现其他成员地址。" : "加入房间后开始发现其他成员。"}</div>
          </Show>
          <For each={diagnosticErrors()}>{(message) => <p class="diagnostic-error">{message}</p>}</For>
        </section>
      </aside>
    </div>
  );
}

function formatRelativeTime(value: string, now: number): string {
  const target = Date.parse(value);
  if (!Number.isFinite(target)) return "等待 announce";
  const seconds = Math.max(0, Math.ceil((target - now) / 1000));
  return `${seconds} 秒后`;
}

function CandidateRow(props: { candidate: Candidate }) {
  const typeLabel = () => {
    if (props.candidate.type === "port-mapped") return "端口映射";
    if (props.candidate.type === "server-reflexive") return "STUN 公网";
    return "本机";
  };
  return (
    <li>
      <b>{typeLabel()}</b>
      <div>
        <code>{props.candidate.address}</code>
        <small>{props.candidate.interface || props.candidate.source || props.candidate.family || ""}</small>
      </div>
    </li>
  );
}
