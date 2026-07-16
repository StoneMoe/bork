import {
  For,
  Index,
  Show,
  createMemo,
  createSignal,
  onCleanup,
} from "solid-js";
import * as Backend from "@wailsjs/go/app/App";
import { ClipboardSetText } from "@wailsjs/runtime/runtime";
import { createRemoteState } from "./sync";
import type { AppState, Candidate, FriendlyStatus, RemotePeer } from "./types";

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
      title: `已连接 ${remotePeers.length} 位成员`,
      detail: state.audio.running
        ? "语音正在通过认证链路传输。"
        : state.audio.error
          ? "成员已连接，但音频设备暂时不可用。"
          : !state.audio.available
            ? "成员已连接，但没有可用的麦克风或扬声器。"
          : "成员已连接，正在自动接通语音。",
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
  const operational = createMemo(() => remote.ready() && Boolean(state().peerId));
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
    <main class="shell" classList={{ busy: busy() }}>
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

      <Settings
        state={state()}
        open={settingsOpen()}
        busy={busy()}
        ready={operational()}
        close={() => setSettingsOpen(false)}
        runAction={runAction}
      />

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
            maxlength={1024}
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
              <header>
                <div><span class="label">ROOM NETWORK</span><strong>房间拓扑</strong></div>
                <b>{remotePeers().length + 1}</b>
              </header>
          <NetworkTopology state={props.state} remotePeers={remotePeers()} />
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

interface TopologyPosition {
  x: number;
  y: number;
}

function topologyPositions(count: number): TopologyPosition[] {
  if (count === 1) return [{ x: 50, y: 17 }];
  return Array.from({ length: count }, (_, index) => {
    const outerCount = Math.min(count, 8);
    const innerCount = count - outerCount;
    const inner = index >= outerCount;
    const ringIndex = inner ? index - outerCount : index;
    const ringCount = inner ? innerCount : outerCount;
    const angle = -Math.PI / 2 + (ringIndex * Math.PI * 2) / ringCount;
    return {
      x: 50 + Math.cos(angle) * (inner ? 24 : 39),
      y: 50 + Math.sin(angle) * (inner ? 24 : 36),
    };
  });
}

function NetworkTopology(props: { state: AppState; remotePeers: RemotePeer[] }) {
  const positions = () => topologyPositions(props.remotePeers.length);
  const audioDetail = () => props.state.audio.running
    ? (props.state.audio.muted ? "运行中，麦克风已静音" : "运行中，正在发送语音")
    : (props.state.audio.error || "尚未运行");

  return (
    <div class="topology-graph" aria-label="当前已认证网络拓扑">
      <svg class="topology-lines" viewBox="0 0 100 100" preserveAspectRatio="none" aria-hidden="true">
        <Index each={props.remotePeers}>{(_, index) => {
          const position = () => positions()[index];
          return <line x1="50" y1="50" x2={position().x} y2={position().y} />;
        }}</Index>
      </svg>

      <div class="topology-node client" tabindex={0} style={{ left: "50%", top: "50%" }}>
        <span class="node-avatar">你</span>
        <strong>本机</strong>
        <small>{props.state.audio.running ? (props.state.audio.muted ? "已静音" : "语音中") : "在线"}</small>
        <div class="topology-tooltip node-tooltip">
          <b>本机 Peer</b>
          <dl>
            <dt>PeerID</dt><dd>{props.state.peerId || "正在载入"}</dd>
            <dt>UDP endpoint</dt><dd>{props.state.room?.localAddress || "尚未打开"}</dd>
            <dt>房间状态</dt><dd>{props.state.room?.phase || "未知"}</dd>
            <dt>音频设备</dt><dd>{props.state.audio.available ? "可用" : "不可用"}</dd>
            <dt>语音状态</dt><dd>{audioDetail()}</dd>
          </dl>
        </div>
      </div>

      <Index each={props.remotePeers}>{(remotePeer, index) => {
        const position = () => positions()[index];
        const midpoint = () => ({ x: (50 + position().x) / 2, y: (50 + position().y) / 2 });
        return <>
          <div
            class="topology-edge-target"
            classList={{ "near-left": midpoint().x < 30, "near-right": midpoint().x > 70, "near-top": midpoint().y < 30 }}
            tabindex={0}
            style={{ left: `${midpoint().x}%`, top: `${midpoint().y}%` }}
            aria-label={`到 ${remotePeer().peerId.slice(0, 14)} 的认证链路`}
          >
            <span>{remotePeer().rttMillis || 1} ms</span>
            <div class="topology-tooltip edge-tooltip">
              <b>认证直连链路</b>
              <dl>
                <dt>传输</dt><dd>UDP / 逐链路 AEAD</dd>
                <dt>远端地址</dt><dd>{remotePeer().address}</dd>
                <dt>RTT</dt><dd>{remotePeer().rttMillis || 1} ms</dd>
                <dt>SessionID</dt><dd>{remotePeer().sessionId || "未知"}</dd>
                <dt>控制面</dt><dd>已认证 / 防重放</dd>
                <dt>实时语音</dt><dd>{props.state.audio.running ? "启用 / 不重传" : "未发送"}</dd>
              </dl>
            </div>
          </div>
          <div
            class="topology-node remote-peer"
            classList={{ "near-left": position().x < 30, "near-right": position().x > 70, "near-top": position().y < 30 }}
            tabindex={0}
            style={{ left: `${position().x}%`, top: `${position().y}%` }}
          >
            <span class="node-avatar">{remotePeer().peerId.slice(0, 1).toUpperCase()}</span>
            <strong>{remotePeer().peerId.slice(0, 10)}</strong>
            <small>{remotePeer().rttMillis || 1} ms</small>
            <div class="topology-tooltip node-tooltip">
              <b>已认证成员</b>
              <dl>
                <dt>PeerID</dt><dd>{remotePeer().peerId}</dd>
                <dt>认证状态</dt><dd>Ed25519 + RoomSeed</dd>
                <dt>远端地址</dt><dd>{remotePeer().address}</dd>
                <dt>RTT</dt><dd>{remotePeer().rttMillis || 1} ms</dd>
                <dt>SessionID</dt><dd>{remotePeer().sessionId || "未知"}</dd>
              </dl>
            </div>
          </div>
        </>;
      }}</Index>
    </div>
  );
}

interface SettingsProps extends ActionProps {
  state: AppState;
  open: boolean;
  close: () => void;
}

function Settings(props: SettingsProps) {
  const audio = () => props.state.audio;
  const diagnostics = () => props.state.diagnostics;
  const candidates = () => diagnostics().candidates;
  const stun = () => diagnostics().stun;
  const diagnosticError = () => diagnostics().networkError || diagnostics().discoveryError || "";

  return (
    <div class="settings-layer" classList={{ open: props.open }} aria-hidden={!props.open}>
      <button class="settings-backdrop" type="button" aria-label="关闭设置" onClick={props.close} />
      <Show when={props.open}>
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
          <div class="setting-row stacked">
            <span>用户身份</span>
            <code>{props.state.peerId || "正在载入"}</code>
          </div>
        </section>
        <section class="settings-section">
          <h3>连接诊断</h3>
          <div class="setting-row stacked">
            <span>共享 UDP 地址</span>
            <code>{diagnostics().listenAddress || "尚未打开"}</code>
          </div>
          <div class="diagnostic-heading"><span>候选地址</span><b>{candidates().length}</b></div>
          <ol class="candidate-list">
            <For each={candidates()}>{(candidate) => <CandidateRow candidate={candidate} />}</For>
          </ol>
          <Show when={candidates().length === 0}>
            <div class="empty-diagnostic">加入房间后开始收集。</div>
          </Show>
          <div class="diagnostic-heading"><span>STUN 探测</span></div>
          <ol class="stun-list">
            <For each={stun()}>{(result) => (
              <li classList={{ ok: Boolean(result.mappedAddress), failed: !result.mappedAddress }} title={result.error || ""}>
                <span>{result.server}</span>
                <b>{result.mappedAddress ? `${result.rttMillis || 1} ms` : "失败"}</b>
              </li>
            )}</For>
          </ol>
          <Show when={diagnosticError()}>
            <p class="diagnostic-error">{diagnosticError()}</p>
          </Show>
        </section>
      </aside>
      </Show>
    </div>
  );
}

function CandidateRow(props: { candidate: Candidate }) {
  return (
    <li>
      <b>{props.candidate.type === "server-reflexive" ? "公网" : "本机"}</b>
      <div>
        <code>{props.candidate.address}</code>
        <small>{props.candidate.interface || props.candidate.source || props.candidate.family || ""}</small>
      </div>
    </li>
  );
}
