import { For, Show, createEffect, createMemo, createSignal, onCleanup, onMount } from "solid-js";
import * as Backend from "@wailsjs/go/app/App";
import { ClipboardSetText, Quit, WindowIsMaximised, WindowMinimise, WindowToggleMaximise } from "@wailsjs/runtime/runtime";
import Room from "./Room";
import Settings from "./Settings";
import { closePopoversEvent, nativePopoverOpen, nativePopoverSupported } from "./popover";
import { parseRoomHistory, roomHistoryStorageKey, withRecentRoom } from "./room-history";
import { createRemoteState } from "./sync";
import type { RoomHistoryEntry } from "./room-history";
import type { ActionProps, AppState, FriendlyStatus } from "./types";

const maxInviteLength = 512;
const nicknameStorageKey = "bork.nickname";
const echoCancellationDisabledStorageKey = "bork.audio.echoCancellation.disabled";
const noiseSuppressionDisabledStorageKey = "bork.audio.noiseSuppression.disabled";
const remoteLoudnessNormalizationDisabledStorageKey = "bork.audio.remoteLoudnessNormalization.disabled";

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
  const [roomHistory, setRoomHistory] = createSignal(readRoomHistory());
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
    } else {
      const name = state().room?.name;
      if (name) {
        void Backend.GetInvite().then((invite) => {
          if (invite) updateRoomHistory(withRecentRoom(roomHistory(), { name, invite, visitedAt: Date.now() }));
        }).catch(() => { /* history is optional */ });
      }
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

  function updateRoomHistory(history: RoomHistoryEntry[]) {
    setRoomHistory(history);
    try {
      if (history.length > 0) localStorage.setItem(roomHistoryStorageKey, JSON.stringify(history));
      else localStorage.removeItem(roomHistoryStorageKey);
    } catch { /* storage unavailable */ }
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
          fallback={
            <Lobby
              busy={busy()}
              ready={operational()}
              history={roomHistory()}
              runAction={runAction}
              removeHistory={(invite) => updateRoomHistory(roomHistory().filter((entry) => entry.invite !== invite))}
            />
          }
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

interface LobbyProps extends ActionProps {
  history: readonly RoomHistoryEntry[];
  removeHistory: (invite: string) => void;
}

type LobbyPage = "home" | "create" | "join";

function Lobby(props: LobbyProps) {
  const [roomName, setRoomName] = createSignal("");
  const [invite, setInvite] = createSignal("");
  const [page, setPage] = createSignal<LobbyPage>("home");
  let createButton: HTMLButtonElement | undefined;
  let joinButton: HTMLButtonElement | undefined;
  let roomNameInput: HTMLInputElement | undefined;
  let inviteInput: HTMLTextAreaElement | undefined;

  function openPage(next: Exclude<LobbyPage, "home">) {
    setPage(next);
    queueMicrotask(() => (next === "create" ? roomNameInput : inviteInput)?.focus({ preventScroll: true }));
  }

  function returnHome() {
    const previous = page();
    if (previous === "home" || props.busy) return;
    setPage("home");
    queueMicrotask(() => (previous === "create" ? createButton : joinButton)?.focus({ preventScroll: true }));
  }

  function handleLobbyKeyDown(event: KeyboardEvent) {
    if (event.key !== "Escape" || page() === "home" || props.busy) return;
    event.preventDefault();
    event.stopPropagation();
    returnHome();
  }

  async function createRoom(event: SubmitEvent) {
    event.preventDefault();
    if (await props.runAction(() => Backend.CreateRoom(roomName().trim()))) setRoomName("");
  }

  async function joinRoom(event: SubmitEvent) {
    event.preventDefault();
    if (await props.runAction(() => Backend.JoinRoom(invite().trim()))) setInvite("");
  }

  return (
    <section class="lobby-view" classList={{ "subview-open": page() !== "home" }} onKeyDown={handleLobbyKeyDown}>
      <Show when={page() === "home"} fallback={
        <section class="lobby-subview" aria-labelledby="lobbySubviewTitle">
          <header class="lobby-subview-header">
            <button class="lobby-subview-back" type="button" disabled={props.busy} aria-label="返回" title="返回" onClick={returnHome}>
              <BackIcon />
            </button>
            <h1 id="lobbySubviewTitle">{page() === "create" ? "创建房间" : "加入房间"}</h1>
          </header>
          <Show when={page() === "create"} fallback={
            <form class="lobby-form join" onSubmit={joinRoom}>
              <textarea
                ref={inviteInput}
                id="inviteInput"
                aria-label="房间邀请"
                aria-describedby="inviteInputDescription"
                maxlength={maxInviteLength}
                spellcheck={false}
                autocomplete="off"
                placeholder="粘贴房间成员发送的邀请链接"
                value={invite()}
                onInput={(event) => setInvite(event.currentTarget.value)}
                required
              />
              <p id="inviteInputDescription" class="lobby-input-description">链接格式：<code>bork://join/…</code></p>
              <button class="lobby-submit" type="submit" disabled={props.busy || !props.ready} aria-label="加入房间">
                <span>进入</span>
                <ChevronIcon />
              </button>
            </form>
          }>
            <form class="lobby-form" onSubmit={createRoom}>
              <input
                ref={roomNameInput}
                id="roomName"
                aria-label="房间名称"
                autocomplete="off"
                placeholder="输入房间名称"
                value={roomName()}
                onInput={(event) => setRoomName(event.currentTarget.value)}
                required
              />
              <button class="lobby-submit" type="submit" disabled={props.busy || !props.ready} aria-label="创建并进入房间">
                <span>进入</span>
                <ChevronIcon />
              </button>
            </form>
          </Show>
        </section>
      }>
        <section class="lobby-panel" aria-label="房间操作">
          <div class="lobby-entry-actions">
            <button
              ref={createButton}
              class="lobby-entry-button create"
              type="button"
              disabled={props.busy || !props.ready}
              onClick={() => openPage("create")}
            >
              <CreateRoomIcon />
              <span>创建房间</span>
              <ChevronIcon />
            </button>
            <button
              ref={joinButton}
              class="lobby-entry-button"
              type="button"
              disabled={props.busy || !props.ready}
              onClick={() => openPage("join")}
            >
              <JoinRoomIcon />
              <span>加入房间</span>
              <ChevronIcon />
            </button>
          </div>
          <Show when={props.history.length > 0}>
            <section class="room-history" aria-labelledby="roomHistoryLabel">
              <div id="roomHistoryLabel" class="room-history-label">最近房间</div>
              <ul class="room-history-list">
                <For each={props.history}>{(room) => (
                  <li class="room-history-item">
                    <button
                      class="room-history-tag"
                      type="button"
                      disabled={props.busy || !props.ready}
                      title={`重新加入 ${room.name} · ${new Date(room.visitedAt).toLocaleString("zh-CN")}`}
                      onClick={() => void props.runAction(() => Backend.JoinRoom(room.invite))}
                    >
                      <HistoryIcon />
                      <span>{room.name}</span>
                    </button>
                    <button
                      class="room-history-remove"
                      type="button"
                      aria-label={`从最近房间移除 ${room.name}`}
                      title="移除记录"
                      onClick={() => props.removeHistory(room.invite)}
                    ><CloseIcon /></button>
                  </li>
                )}</For>
              </ul>
            </section>
          </Show>
        </section>
      </Show>
    </section>
  );
}

function CreateRoomIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>;
}

function JoinRoomIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M14 5h5v14h-5M5 12h10M11 8l4 4-4 4" /></svg>;
}

function ChevronIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m9 6 6 6-6 6" /></svg>;
}

function HistoryIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4.5 12a7.5 7.5 0 1 0 2.2-5.3L4.5 9M4.5 4.5V9H9M12 8v4.5l3 1.8" /></svg>;
}

function readRoomHistory() {
  try {
    return parseRoomHistory(localStorage.getItem(roomHistoryStorageKey));
  } catch {
    return [];
  }
}
