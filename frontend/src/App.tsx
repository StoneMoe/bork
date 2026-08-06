import { Show, createEffect, createMemo, createSignal, onCleanup, onMount } from "solid-js";
import * as Backend from "@wailsjs/go/app/App";
import { ClipboardSetText, Quit, WindowIsMaximised, WindowMinimise, WindowToggleMaximise } from "@wailsjs/runtime/runtime";
import Room from "./Room";
import Settings from "./Settings";
import { closePopoversEvent, nativePopoverOpen, nativePopoverSupported } from "./popover";
import { createRemoteState } from "./sync";
import type { ActionProps, AppState, FriendlyStatus } from "./types";

const maxInviteLength = 512;
const nicknameStorageKey = "bork.nickname";
const echoCancellationDisabledStorageKey = "bork.audio.echoCancellation.disabled";
const noiseSuppressionDisabledStorageKey = "bork.audio.noiseSuppression.disabled";
const remoteLoudnessNormalizationDisabledStorageKey = "bork.audio.remoteLoudnessNormalization.disabled";
const lobbySpectrum = [14, 22, 38, 58, 32, 70, 48, 82, 60, 92, 100, 92, 60, 82, 48, 70, 32, 58, 38, 22, 14];

function hasNativeWindowBridge() {
  const host = window as typeof window & {
    chrome?: { webview?: { postMessage?: unknown } };
    webkit?: { messageHandlers?: { external?: { postMessage?: unknown } } };
  };
  return typeof host.chrome?.webview?.postMessage === "function"
    || typeof host.webkit?.messageHandlers?.external?.postMessage === "function";
}

function closeOpenPopovers() {
  const focused = document.activeElement;
  document.dispatchEvent(new Event(closePopoversEvent));
  if (nativePopoverSupported) {
    document.querySelectorAll<HTMLElement>("[popover]").forEach((popover) => {
      if (nativePopoverOpen(popover)) popover.hidePopover();
    });
  }
  if (focused instanceof HTMLElement && focused.isConnected) focused.focus({ preventScroll: true });
}

function humanStatus(state: AppState): FriendlyStatus {
  if (!state.peerId || !state.room) return {};
  if (state.room.phase === "gathering") {
    return {
      title: "正在准备连接",
      detail: "Bork 正在打开通信端口并检查网络环境。",
    };
  }
  if (state.room.remotePeers.length > 0) return {};
  return {
    title: "正在寻找房间成员",
    detail: "保持 Bork 运行，其他成员上线后会自动尝试连接。",
  };
}

export default function App() {
  const [busy, setBusy] = createSignal(false);
  const [settingsOpen, setSettingsOpen] = createSignal(false);
  const [inviteCopied, setInviteCopied] = createSignal(false);
  const [nicknameStorageReady, setNicknameStorageReady] = createSignal(false);
  const [audioPreferencesStorageReady, setAudioPreferencesStorageReady] = createSignal(false);
  const customWindowControls = hasNativeWindowBridge();
  const [windowMaximised, setWindowMaximised] = createSignal(false);
  const [error, setError] = createSignal("");
  let leaveRoomAction: (() => Promise<void>) | undefined;
  let nicknameRestoreStarted = false;
  let audioPreferencesRestoreStarted = false;
  let copyTimer: number | undefined;
  let windowStateGeneration = 0;
  let mainView: HTMLElement | undefined;
  let settingsButton: HTMLButtonElement | undefined;
  const remote = createRemoteState(setError);
  const state = remote.state;
  const operational = createMemo(() => Boolean(state().peerId));
  const inRoom = createMemo(() => Boolean(state().room));
  const friendly = createMemo(() => humanStatus(state()));
  let previousRoomState = inRoom();
  onCleanup(() => window.clearTimeout(copyTimer));
  onMount(() => {
    if (!customWindowControls) return;
    let active = true;
    let resizeTimer: number | undefined;
    const syncWindowState = async () => {
      const generation = ++windowStateGeneration;
      try {
        const maximised = await WindowIsMaximised();
        if (active && generation === windowStateGeneration) setWindowMaximised(maximised);
      } catch { /* runtime unavailable in browser previews */ }
    };
    const syncAfterResize = () => {
      window.clearTimeout(resizeTimer);
      resizeTimer = window.setTimeout(() => void syncWindowState(), 75);
    };
    void syncWindowState();
    window.addEventListener("resize", syncAfterResize);
    window.addEventListener("focus", syncWindowState);
    onCleanup(() => {
      active = false;
      window.clearTimeout(resizeTimer);
      window.removeEventListener("resize", syncAfterResize);
      window.removeEventListener("focus", syncWindowState);
    });
  });
  createEffect(() => {
    const current = inRoom();
    if (current === previousRoomState) return;
    previousRoomState = current;
    if (!current) {
      window.clearTimeout(copyTimer);
      setInviteCopied(false);
    }
    if (!settingsOpen()) queueMicrotask(() => mainView?.focus({ preventScroll: true }));
  });
  createEffect(() => {
    if (!operational() || nicknameRestoreStarted) return;
    nicknameRestoreStarted = true;
    let stored = "";
    try {
      stored = localStorage.getItem(nicknameStorageKey) || "";
    } catch {
      setNicknameStorageReady(true);
      return;
    }
    if (!stored || stored === state().nickname) {
      setNicknameStorageReady(true);
      return;
    }
    void runAction(() => Backend.SetNickname(stored)).then((restored) => {
      if (!restored) {
        try { localStorage.removeItem(nicknameStorageKey); } catch { /* storage unavailable */ }
      }
      setNicknameStorageReady(true);
    });
  });
  createEffect(() => {
    if (!nicknameStorageReady()) return;
    const nickname = state().nickname;
    try {
      if (nickname) localStorage.setItem(nicknameStorageKey, nickname);
      else localStorage.removeItem(nicknameStorageKey);
    } catch { /* storage unavailable */ }
  });
  createEffect(() => {
    if (!operational() || !nicknameStorageReady() || busy() || audioPreferencesRestoreStarted) return;
    audioPreferencesRestoreStarted = true;
    let echoCancellation = true;
    let noiseSuppression = true;
    let remoteLoudnessNormalization = true;
    try {
      echoCancellation = localStorage.getItem(echoCancellationDisabledStorageKey) !== "1";
      noiseSuppression = localStorage.getItem(noiseSuppressionDisabledStorageKey) !== "1";
      remoteLoudnessNormalization = localStorage.getItem(remoteLoudnessNormalizationDisabledStorageKey) !== "1";
    } catch {
      setAudioPreferencesStorageReady(true);
      return;
    }
    const restoreEchoCancellation = echoCancellation !== state().audio.echoCancellation;
    const restoreNoiseSuppression = noiseSuppression !== state().audio.noiseSuppression;
    const restoreRemoteLoudnessNormalization = remoteLoudnessNormalization !== state().audio.remoteLoudnessNormalization;
    if (!restoreEchoCancellation && !restoreNoiseSuppression && !restoreRemoteLoudnessNormalization) {
      setAudioPreferencesStorageReady(true);
      return;
    }
    void runAction(async () => {
      if (restoreEchoCancellation) await Backend.SetEchoCancellation(echoCancellation);
      if (restoreNoiseSuppression) await Backend.SetNoiseSuppression(noiseSuppression);
      if (restoreRemoteLoudnessNormalization) await Backend.SetRemoteLoudnessNormalization(remoteLoudnessNormalization);
    }).then((restored) => {
      if (restored) setAudioPreferencesStorageReady(true);
      else audioPreferencesRestoreStarted = false;
    });
  });
  createEffect(() => {
    if (!audioPreferencesStorageReady()) return;
    try {
      if (state().audio.echoCancellation) localStorage.removeItem(echoCancellationDisabledStorageKey);
      else localStorage.setItem(echoCancellationDisabledStorageKey, "1");
      if (state().audio.noiseSuppression) localStorage.removeItem(noiseSuppressionDisabledStorageKey);
      else localStorage.setItem(noiseSuppressionDisabledStorageKey, "1");
      if (state().audio.remoteLoudnessNormalization) localStorage.removeItem(remoteLoudnessNormalizationDisabledStorageKey);
      else localStorage.setItem(remoteLoudnessNormalizationDisabledStorageKey, "1");
    } catch { /* storage unavailable */ }
  });

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

  function openSettings() {
    closeOpenPopovers();
    setSettingsOpen(true);
  }

  function toggleWindowMaximised() {
    windowStateGeneration++;
    setWindowMaximised((maximised) => !maximised);
    WindowToggleMaximise();
  }

  function closeSettings() {
    setSettingsOpen(false);
    queueMicrotask(() => settingsButton?.focus({ preventScroll: true }));
  }

  async function copyInvite() {
    const copied = await runAction(async () => {
      const invite = await Backend.GetInvite();
      if (!invite) throw new Error("当前没有房间邀请");
      if (!await ClipboardSetText(invite)) throw new Error("无法写入系统剪贴板");
    });
    if (!copied) return;
    setInviteCopied(true);
    window.clearTimeout(copyTimer);
    copyTimer = window.setTimeout(() => setInviteCopied(false), 1600);
  }

  return (
    <main class="shell" classList={{ maximised: customWindowControls && windowMaximised() }}>
      <header
        class="topbar"
        onDblClick={(event) => {
          if (customWindowControls && event.target instanceof Element && !event.target.closest("button")) toggleWindowMaximised();
        }}
      >
        <div class="topbar-leading">
          <Show when={inRoom()}>
            <button
              class="topbar-icon-button back-button"
              type="button"
              disabled={busy() || !operational()}
              aria-label="离开房间"
              title="离开房间"
              onClick={() => void leaveRoomAction?.()}
            ><BackIcon /></button>
          </Show>
          <div class="wordmark">
            BORK<span>/</span>
            <span classList={{ "room-name": inRoom() }}>
              {state().room?.name || "VOICE"}
            </span>
            <Show when={inRoom()}>
              <button
                class="topbar-icon-button copy-invite-button"
                classList={{ copied: inviteCopied() }}
                type="button"
                disabled={busy() || !operational()}
                aria-label={inviteCopied() ? "邀请已复制" : "复制房间邀请"}
                title={inviteCopied() ? "邀请已复制" : "复制房间邀请"}
                onClick={() => void copyInvite()}
              >
                <Show when={inviteCopied()} fallback={<CopyIcon />}><CheckIcon /></Show>
              </button>
            </Show>
          </div>
        </div>
        <div class="topbar-actions">
          <button ref={settingsButton} class="topbar-icon-button settings-button" type="button" disabled={!operational()} aria-label="打开设置" title="设置" onClick={openSettings}>
            <SettingsIcon />
          </button>
          <Show when={customWindowControls}>
            <div class="window-controls" role="group" aria-label="窗口控制">
              <button class="window-control-button" type="button" aria-label="最小化窗口" title="最小化" onClick={WindowMinimise}><MinimiseIcon /></button>
              <button
                class="window-control-button"
                type="button"
                aria-label={windowMaximised() ? "还原窗口" : "最大化窗口"}
                title={windowMaximised() ? "还原" : "最大化"}
                onClick={toggleWindowMaximised}
              >
                <Show when={windowMaximised()} fallback={<MaximiseIcon />}><RestoreIcon /></Show>
              </button>
              <button class="window-control-button close" type="button" aria-label="关闭窗口" title="关闭" onClick={Quit}><CloseIcon /></button>
            </div>
          </Show>
        </div>
        <span class="visually-hidden" role="status" aria-live="polite">{inviteCopied() ? "房间邀请已复制" : ""}</span>
      </header>

      <section ref={mainView} class="main-view" tabindex="-1">
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
            reportError={setError}
            registerLeaveAction={(action) => { leaveRoomAction = action; }}
          />
        </Show>
      </section>

      <Show when={settingsOpen()}>
        <Settings
          state={state()}
          busy={busy()}
          ready={operational()}
          close={closeSettings}
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

function BackIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m12.25 5-7 7 7 7M5.75 12h13" /></svg>;
}

function CopyIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="8.5" y="8.5" width="11" height="11" rx="2" /><path d="M16.5 8.5v-2a2 2 0 0 0-2-2h-8a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2" /></svg>;
}

function CheckIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12.5 4.5 4.5L19 7.5" /></svg>;
}

function SettingsIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h10M18 7h2M4 17h2M10 17h10M14 4v6M6 14v6" /></svg>;
}

function MinimiseIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 17h12" /></svg>;
}

function MaximiseIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="6.5" y="6.5" width="11" height="11" rx="1" /></svg>;
}

function RestoreIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M8 8V6.5h9.5V16H16M6.5 8H16v9.5H6.5z" /></svg>;
}

function CloseIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>;
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
      <div class="lobby-visual" aria-hidden="true">
        <div class="lobby-spectrum">
          {lobbySpectrum.map((level, index) => <i style={`--level:${level}%;--index:${index}`} />)}
        </div>
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
