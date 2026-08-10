import { For, Show, createEffect, createSignal, onCleanup, onMount } from "solid-js";
import * as Backend from "@wailsjs/go/app/App";
import { closePopoversEvent, nativePopoverOpen, nativePopoverSupported } from "./popover";
import type { ActionProps, AppState, FileTransfer, RemotePeer } from "./types";

const memberInfoPopoverID = "member-info-popover";
const fileSharePopoverID = "file-share-popover";

interface RoomControlRowProps extends ActionProps {
  state: AppState;
  remotePeers: RemotePeer[];
  screenSharing: boolean;
  captureBusy: boolean;
  remoteSharerCount: number;
  focusFallback: () => void;
  toggleScreenShare: () => void;
}

export function RoomControlRow(props: RoomControlRowProps) {
  const [fileShareOpen, setFileShareOpen] = createSignal(false);
  let fileShareButton: HTMLButtonElement | undefined;
  const incomingFile = () => props.state.room?.transfers.some((transfer) => transfer.direction === "incoming" && transfer.status === "offered");

  return (
    <>
      <div class="room-control-row" role="group" aria-label="房间控制">
        <div class="room-feature-bar" role="group" aria-label="房间功能">
          <button
            class="feature-button"
            classList={{ active: props.screenSharing }}
            type="button"
            disabled={props.busy || !props.ready || props.captureBusy}
            aria-label={props.screenSharing ? "停止屏幕分享" : props.captureBusy ? "选择要分享的屏幕" : "开始屏幕分享"}
            aria-pressed={props.screenSharing}
            title={props.screenSharing ? "停止屏幕分享" : "开始屏幕分享"}
            onClick={props.toggleScreenShare}
          >
            <ScreenShareIcon />
            <span>
              <strong>{props.screenSharing ? "停止分享" : props.captureBusy ? "选择屏幕…" : "屏幕分享"}</strong>
              <small>{props.remoteSharerCount ? `${props.remoteSharerCount} 人正在分享` : props.screenSharing ? "你的画面正在共享" : "选择窗口或屏幕"}</small>
            </span>
          </button>
          <button
            ref={fileShareButton}
            class="feature-button file-feature-button"
            classList={{ attention: Boolean(incomingFile()), open: fileShareOpen() }}
            type="button"
            disabled={props.busy || !props.ready}
            aria-label={incomingFile() ? "打开文件分享，有待接收文件" : "打开文件分享"}
            aria-expanded={fileShareOpen()}
            title="打开文件分享"
            popovertarget={nativePopoverSupported ? fileSharePopoverID : undefined}
            popovertargetaction={nativePopoverSupported ? "toggle" : undefined}
            onClick={() => {
              if (nativePopoverSupported) return;
              if (!fileShareOpen()) document.dispatchEvent(new Event(closePopoversEvent));
              setFileShareOpen((open) => !open);
            }}
          >
            <FileShareIcon />
            <span>
              <strong>文件分享</strong>
              <small>{props.state.room?.transfers.length ? `${props.state.room.transfers.length} 条记录` : "选择成员发送"}</small>
            </span>
          </button>
        </div>
        <div class="voice-controls" role="group" aria-label="语音控制">
          <AudioControl
            kind="capture"
            label="麦克风"
            muted={props.state.audio.captureMuted}
            gain={props.state.audio.captureGain}
            disabled={props.busy || !props.ready || !props.state.audio.available}
            setMuted={(muted) => props.runAction(() => Backend.SetCaptureMuted(muted))}
            setGain={(gain) => props.runAction(() => Backend.SetCaptureGain(gain))}
          />
          <AudioControl
            kind="playback"
            label="扬声器"
            muted={props.state.audio.playbackMuted}
            gain={props.state.audio.playbackGain}
            disabled={props.busy || !props.ready || !props.state.audio.available}
            setMuted={(muted) => props.runAction(() => Backend.SetPlaybackMuted(muted))}
            setGain={(gain) => props.runAction(() => Backend.SetPlaybackGain(gain))}
          />
        </div>
      </div>
      <FileSharePopover
        state={props.state}
        remotePeers={props.remotePeers}
        busy={props.busy || !props.ready}
        runAction={props.runAction}
        open={fileShareOpen()}
        setOpen={setFileShareOpen}
        trigger={() => fileShareButton}
        focusFallback={props.focusFallback}
      />
    </>
  );
}

export function RoomMemberList(props: { state: AppState; remotePeers: RemotePeer[]; focusFallback: () => void }) {
  const [selectedMember, setSelectedMember] = createSignal("");
  const [memberInfoOpen, setMemberInfoOpen] = createSignal(false);
  const [focusedMember, setFocusedMember] = createSignal("");
  let memberInfoCard: HTMLElement | undefined;
  let memberInfoCloseButton: HTMLButtonElement | undefined;
  let memberInfoInvoker: HTMLButtonElement | undefined;
  let memberList: HTMLDivElement | undefined;
  let localNameButton: HTMLButtonElement | undefined;
  let memberInfoGeneration = 0;
  const localSpeaking = () => props.state.audio.speaking && !props.state.audio.captureMuted;
  const remoteSpeaking = (remotePeer: RemotePeer) => !remotePeer.muted && props.state.audio.speakingPeerIds.includes(remotePeer.peerId);
  const remoteName = (remotePeer: RemotePeer) => remotePeer.nickname || remotePeer.peerId.slice(0, 14);
  const memberStatus = (captureMuted: boolean, playbackMuted: boolean, screenSharing: boolean) => [
    captureMuted ? "麦克风已静音" : "",
    playbackMuted ? "扬声器已静音" : "",
    screenSharing ? "分享屏幕" : "",
  ].filter(Boolean).join(" · ");
  const localStatus = () => memberStatus(props.state.audio.captureMuted, props.state.audio.playbackMuted, Boolean(props.state.room?.screenSharing)) || "在线";
  const remoteStatus = (remotePeer: RemotePeer) => memberStatus(remotePeer.muted, remotePeer.playbackMuted, remotePeer.screenSharing) || "在线";
  const remoteTransport = (remotePeer: RemotePeer) => remotePeer.transport === "bridge" ? "桥接" : "直连";
  const selectedRemote = () => props.remotePeers.find((peer) => peer.peerId === selectedMember());
  const memberInfoIsOpen = () => nativePopoverSupported ? nativePopoverOpen(memberInfoCard) : memberInfoOpen();

  function focusMemberInfoClose(generation: number) {
    queueMicrotask(() => {
      if (generation === memberInfoGeneration && memberInfoIsOpen()) {
        memberInfoCloseButton?.focus({ preventScroll: true });
      }
    });
  }

  function restoreMemberFocus(invoker: HTMLButtonElement | undefined, generation: number) {
    queueMicrotask(() => {
      if (generation !== memberInfoGeneration || memberInfoIsOpen()) return;
      const active = document.activeElement;
      if (active && active !== document.body && active.isConnected && !memberInfoCard?.contains(active)) return;
      if (invoker?.isConnected) invoker.focus({ preventScroll: true });
      else if (localNameButton?.isConnected) localNameButton.focus({ preventScroll: true });
      else props.focusFallback();
    });
  }

  function finishMemberInfoClose(generation: number) {
    setMemberInfoOpen(false);
    queueMicrotask(() => {
      if (generation !== memberInfoGeneration || memberInfoIsOpen()) return;
      const invoker = memberInfoInvoker;
      memberInfoInvoker = undefined;
      setSelectedMember("");
      restoreMemberFocus(invoker, generation);
    });
  }

  function openMemberInfo(memberID: string, invoker: HTMLButtonElement) {
    const generation = ++memberInfoGeneration;
    memberInfoInvoker = invoker;
    setSelectedMember(memberID);
    if (nativePopoverSupported) {
      if (!nativePopoverOpen(memberInfoCard)) memberInfoCard?.showPopover();
    } else {
      document.dispatchEvent(new Event(closePopoversEvent));
      setMemberInfoOpen(true);
    }
    focusMemberInfoClose(generation);
  }

  function hideMemberInfo() {
    if (!memberInfoIsOpen()) return;
    if (nativePopoverSupported) memberInfoCard?.hidePopover();
    else finishMemberInfoClose(memberInfoGeneration);
  }

  createEffect(() => {
    const selected = selectedMember();
    if (!selected || selected === "local" || props.remotePeers.some((peer) => peer.peerId === selected)) return;
    if (memberInfoIsOpen()) hideMemberInfo();
    else finishMemberInfoClose(memberInfoGeneration);
  });

  createEffect(() => {
    const focused = focusedMember();
    if (!focused || props.remotePeers.some((peer) => peer.peerId === focused)) return;
    setFocusedMember("");
    queueMicrotask(() => {
      const active = document.activeElement;
      if (active && active !== document.body && active.isConnected) return;
      if (localNameButton?.isConnected) localNameButton.focus({ preventScroll: true });
      else props.focusFallback();
    });
  });

  onMount(() => {
    const close = () => hideMemberInfo();
    const dismissOnEscape = (event: KeyboardEvent) => {
      if (nativePopoverSupported || event.key !== "Escape" || !memberInfoIsOpen()) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      hideMemberInfo();
    };
    const dismissOutside = (event: PointerEvent) => {
      if (nativePopoverSupported || !memberInfoIsOpen() || !memberInfoCard || !(event.target instanceof Node)) return;
      if (!memberInfoCard.contains(event.target)) hideMemberInfo();
    };
    document.addEventListener(closePopoversEvent, close);
    document.addEventListener("keydown", dismissOnEscape);
    document.addEventListener("pointerdown", dismissOutside);
    onCleanup(() => {
      document.removeEventListener(closePopoversEvent, close);
      document.removeEventListener("keydown", dismissOnEscape);
      document.removeEventListener("pointerdown", dismissOutside);
    });
  });

  onCleanup(() => {
    ++memberInfoGeneration;
    const active = document.activeElement;
    const focusIsLeaving = Boolean(active && (memberList?.contains(active) || memberInfoCard?.contains(active)));
    if (nativePopoverOpen(memberInfoCard)) memberInfoCard?.hidePopover();
    if (focusIsLeaving) props.focusFallback();
  });

  return (
    <>
      <div ref={memberList} class="member-list" role="list" aria-label="当前房间成员">
        <div class="member-row local-member" role="listitem" classList={{ speaking: localSpeaking() }}>
          <span class="member-identity">
            <span class="member-name-line">
              <button
                ref={localNameButton}
                class="member-name member-name-trigger"
                type="button"
                aria-label={`查看 ${props.state.nickname || "本机"} 的详细信息`}
                aria-controls={memberInfoPopoverID}
                aria-haspopup="dialog"
                aria-expanded={memberInfoOpen() && selectedMember() === "local"}
                onClick={(event) => openMemberInfo("local", event.currentTarget)}
              >{props.state.nickname || "本机"}</button>
              <Show when={props.state.audio.captureMuted}>
                <span class="member-muted-icon" role="img" aria-label="麦克风已静音" title="麦克风已静音"><MicrophoneIcon muted /></span>
              </Show>
              <Show when={props.state.audio.playbackMuted}>
                <span class="member-muted-icon" role="img" aria-label="扬声器已静音" title="扬声器已静音"><SpeakerIcon muted /></span>
              </Show>
              <Show when={props.state.room?.screenSharing}>
                <span class="member-status-icon" role="img" aria-label="正在分享屏幕" title="正在分享屏幕"><ScreenShareIcon /></span>
              </Show>
            </span>
            <span class="visually-hidden">{localSpeaking() ? "正在说话" : "未在说话"}</span>
          </span>
          <span class="member-network">
            <strong class="member-connection local">本机</strong>
          </span>
        </div>
        <For each={props.remotePeers.map((peer) => peer.peerId)}>{(peerID) => {
          const peer = () => props.remotePeers.find((candidate) => candidate.peerId === peerID)!;
          return (
            <div class="member-row" role="listitem" classList={{ speaking: remoteSpeaking(peer()) }}>
              <span class="member-identity">
                <span class="member-name-line">
                  <button
                    class="member-name member-name-trigger"
                    type="button"
                    aria-label={`查看 ${remoteName(peer())} 的详细信息`}
                    aria-controls={memberInfoPopoverID}
                    aria-haspopup="dialog"
                    aria-expanded={memberInfoOpen() && selectedMember() === peerID}
                    onFocus={() => setFocusedMember(peerID)}
                    onBlur={(event) => {
                      const button = event.currentTarget;
                      queueMicrotask(() => {
                        if (button.isConnected && focusedMember() === peerID) setFocusedMember("");
                      });
                    }}
                    onClick={(event) => openMemberInfo(peerID, event.currentTarget)}
                  >{remoteName(peer())}</button>
                  <Show when={peer().muted}>
                    <span class="member-muted-icon" role="img" aria-label="麦克风已静音" title="麦克风已静音"><MicrophoneIcon muted /></span>
                  </Show>
                  <Show when={peer().playbackMuted}>
                    <span class="member-muted-icon" role="img" aria-label="扬声器已静音" title="扬声器已静音"><SpeakerIcon muted /></span>
                  </Show>
                  <Show when={peer().screenSharing}>
                    <span class="member-status-icon" role="img" aria-label="正在分享屏幕" title="正在分享屏幕"><ScreenShareIcon /></span>
                  </Show>
                </span>
                <span class="visually-hidden">{remoteSpeaking(peer()) ? "正在说话" : "未在说话"}</span>
              </span>
              <span class="member-network">
                <strong class="member-connection" classList={{ bridge: peer().transport === "bridge" }}>{remoteTransport(peer())}</strong>
                <small class="member-latency">{peer().rttMillis || 1} ms</small>
              </span>
            </div>
          );
        }}</For>
      </div>
      <aside
        ref={memberInfoCard}
        id={memberInfoPopoverID}
        class="floating-card member-info-card"
        classList={{ "fallback-popover": !nativePopoverSupported, "fallback-open": memberInfoOpen() }}
        popover={nativePopoverSupported ? "auto" : undefined}
        role="dialog"
        aria-labelledby="member-info-title"
        onToggle={(event) => {
          if (!nativePopoverSupported) return;
          if (event.newState === "open" || nativePopoverOpen(memberInfoCard)) {
            setMemberInfoOpen(true);
            focusMemberInfoClose(memberInfoGeneration);
          } else {
            finishMemberInfoClose(memberInfoGeneration);
          }
        }}
      >
        <header>
          <div>
            <small>PEER INFO</small>
            <strong id="member-info-title">{selectedMember() === "local" ? props.state.nickname || "本机" : selectedRemote() ? remoteName(selectedRemote()!) : "成员已离开"}</strong>
          </div>
          <button
            ref={memberInfoCloseButton}
            type="button"
            autofocus
            onClick={hideMemberInfo}
          >关闭</button>
        </header>
        <Show when={selectedMember() === "local"} fallback={selectedRemote() && (
          <div class="member-info-grid">
            <span><small>昵称</small><b>{remoteName(selectedRemote()!)}</b></span>
            <span><small>PeerID</small><code>{selectedRemote()!.peerId}</code></span>
            <span><small>Session</small><code>{selectedRemote()!.sessionId || "未知"}</code></span>
            <span><small>连接</small><b>{remoteTransport(selectedRemote()!)} · {selectedRemote()!.rttMillis || 1} ms</b></span>
            <span><small>{selectedRemote()!.transport === "bridge" ? "下一跳" : "远端地址"}</small><code>{selectedRemote()!.address}</code></span>
            <span><small>状态</small><b>{remoteStatus(selectedRemote()!)}</b></span>
          </div>
        )}>
          <div class="member-info-grid">
            <span><small>昵称</small><b>{props.state.nickname || "本机"}</b></span>
            <span><small>PeerID</small><code>{props.state.peerId || "正在载入"}</code></span>
            <span><small>本机端点</small><code>{props.state.diagnostics.listenAddress || "尚未打开"}</code></span>
            <span><small>房间状态</small><b>{props.state.room?.phase || "未知"}</b></span>
            <span><small>音频状态</small><b>{localStatus()}</b></span>
          </div>
        </Show>
      </aside>
    </>
  );
}

function FileSharePopover(props: {
  state: AppState;
  remotePeers: RemotePeer[];
  busy: boolean;
  runAction: ActionProps["runAction"];
  open: boolean;
  setOpen: (open: boolean) => void;
  trigger: () => HTMLButtonElement | undefined;
  focusFallback: () => void;
}) {
  const [focusedRecipient, setFocusedRecipient] = createSignal("");
  let fileShareCard: HTMLElement | undefined;
  let fileShareCloseButton: HTMLButtonElement | undefined;
  const remoteName = (peer: RemotePeer) => peer.nickname || peer.peerId.slice(0, 14);
  const fileShareIsOpen = () => nativePopoverSupported ? nativePopoverOpen(fileShareCard) : props.open;

  function restoreFileShareFocus() {
    queueMicrotask(() => {
      const active = document.activeElement;
      if (active && active !== document.body && active.isConnected && !fileShareCard?.contains(active)) return;
      const trigger = props.trigger();
      if (trigger?.isConnected) trigger.focus({ preventScroll: true });
      else props.focusFallback();
    });
  }

  function closeFileShare() {
    if (!fileShareIsOpen()) return;
    if (nativePopoverSupported) fileShareCard?.hidePopover();
    else props.setOpen(false);
    restoreFileShareFocus();
  }

  function focusFileShareClose() {
    queueMicrotask(() => {
      if (fileShareIsOpen()) fileShareCloseButton?.focus({ preventScroll: true });
    });
  }

  createEffect(() => {
    const focusedPeerID = focusedRecipient();
    if (!focusedPeerID || props.remotePeers.some((peer) => peer.peerId === focusedPeerID)) return;
    setFocusedRecipient("");
    queueMicrotask(() => {
      if (!fileShareIsOpen()) return props.focusFallback();
      if (props.remotePeers.length > 0) return fileShareCloseButton?.focus({ preventScroll: true });
      closeFileShare();
    });
  });

  createEffect(() => {
    if (!nativePopoverSupported && props.open) focusFileShareClose();
  });

  onMount(() => {
    const close = () => closeFileShare();
    const dismissOnEscape = (event: KeyboardEvent) => {
      if (nativePopoverSupported || event.key !== "Escape" || !fileShareIsOpen()) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      closeFileShare();
    };
    const dismissOutside = (event: PointerEvent) => {
      if (nativePopoverSupported || !fileShareIsOpen() || !fileShareCard || !(event.target instanceof Node)) return;
      const trigger = props.trigger();
      if (!fileShareCard.contains(event.target) && !trigger?.contains(event.target)) closeFileShare();
    };
    document.addEventListener(closePopoversEvent, close);
    document.addEventListener("keydown", dismissOnEscape);
    document.addEventListener("pointerdown", dismissOutside);
    onCleanup(() => {
      document.removeEventListener(closePopoversEvent, close);
      document.removeEventListener("keydown", dismissOnEscape);
      document.removeEventListener("pointerdown", dismissOutside);
    });
  });

  onCleanup(() => {
    const active = document.activeElement;
    const focusIsLeaving = Boolean(active && (fileShareCard?.contains(active) || active === props.trigger()));
    if (nativePopoverOpen(fileShareCard)) fileShareCard?.hidePopover();
    if (focusIsLeaving) props.focusFallback();
  });

  return (
    <section
      ref={fileShareCard}
      id={fileSharePopoverID}
      class="floating-card file-share-card"
      classList={{ "fallback-popover": !nativePopoverSupported, "fallback-open": props.open }}
      popover={nativePopoverSupported ? "auto" : undefined}
      role="dialog"
      aria-labelledby="file-share-title"
      aria-describedby="file-share-description"
      onToggle={(event) => {
        if (!nativePopoverSupported) return;
        const open = event.newState === "open" || nativePopoverOpen(fileShareCard);
        props.setOpen(open);
        if (open) focusFileShareClose();
        else setFocusedRecipient("");
      }}
    >
      <header>
        <div><strong id="file-share-title">发送文件</strong><small id="file-share-description">选择一个已认证成员，然后选择本机文件。</small></div>
        <button
          ref={fileShareCloseButton}
          type="button"
          autofocus
          popovertarget={nativePopoverSupported ? fileSharePopoverID : undefined}
          popovertargetaction={nativePopoverSupported ? "hide" : undefined}
          onClick={() => { if (!nativePopoverSupported) closeFileShare(); }}
        >关闭</button>
      </header>
      <div class="file-recipient-list">
        <Show when={props.remotePeers.length > 0} fallback={<p class="file-recipient-empty">正在等待可发送文件的房间成员。</p>}>
          <For each={props.remotePeers.map((peer) => peer.peerId)}>{(peerID) => {
            const peer = () => props.remotePeers.find((candidate) => candidate.peerId === peerID)!;
            return (
              <button
                type="button"
                disabled={props.busy}
                onFocus={() => setFocusedRecipient(peerID)}
                onBlur={() => queueMicrotask(() => {
                  if (!props.busy && focusedRecipient() === peerID) setFocusedRecipient("");
                })}
                onClick={async (event) => {
                  const button = event.currentTarget;
                  await props.runAction(async () => { await Backend.OfferFile(peerID); });
                  queueMicrotask(() => {
                    const active = document.activeElement;
                    if (active === button) {
                      setFocusedRecipient(peerID);
                      return;
                    }
                    setFocusedRecipient("");
                    if (active && active !== document.body && active.isConnected) return;
                    if (button.isConnected && !button.disabled) button.focus({ preventScroll: true });
                    else if (fileShareIsOpen()) fileShareCloseButton?.focus({ preventScroll: true });
                    else {
                      const trigger = props.trigger();
                      if (trigger?.isConnected) trigger.focus({ preventScroll: true });
                      else props.focusFallback();
                    }
                  });
                }}
              >
                <span><strong>{remoteName(peer())}</strong><small>{peer().transport === "bridge" ? "桥接" : "直连"} · {peer().rttMillis || 1} ms</small></span>
                <b>选择文件</b>
              </button>
            );
          }}</For>
        </Show>
      </div>
      <TransferPanel
        state={props.state}
        busy={props.busy}
        runAction={props.runAction}
        focusFallback={() => {
          if (fileShareCloseButton?.isConnected) fileShareCloseButton.focus({ preventScroll: true });
          else props.focusFallback();
        }}
      />
    </section>
  );
}

function TransferPanel(props: { state: AppState; busy: boolean; runAction: ActionProps["runAction"]; focusFallback: () => void }) {
  const transfers = () => props.state.room?.transfers ?? [];
  const localName = () => props.state.nickname || "本机";
  const peerName = (transfer: FileTransfer) => transfer.peerNickname || transfer.peerId.slice(0, 14);
  const status = (transfer: FileTransfer) => ({
    preparing: "正在准备", offered: transfer.direction === "incoming" ? "等待接收" : "等待对方接收",
    accepting: "正在创建文件", transferring: "传输中", waiting: "等待确认",
    completed: "已完成", rejected: "已拒绝", canceled: "已取消", failed: "失败",
  }[transfer.status] || transfer.status);
  const terminal = (transfer: FileTransfer) => ["completed", "rejected", "canceled", "failed"].includes(transfer.status);

  async function runTransferAction(button: HTMLButtonElement, action: () => Promise<void>) {
    await props.runAction(action);
    queueMicrotask(() => {
      const active = document.activeElement;
      if (active && active !== document.body && active.isConnected) return;
      if (button.isConnected && !button.disabled) button.focus({ preventScroll: true });
      else props.focusFallback();
    });
  }

  return (
    <Show when={transfers().length > 0}>
      <section class="transfer-panel" aria-label="文件传输">
        <header><strong>文件传输</strong><span>{transfers().length}</span></header>
        <For each={transfers().map((transfer) => transfer.id)}>{(transferID) => {
          const transfer = () => transfers().find((candidate) => candidate.id === transferID)!;
          const incoming = () => transfer().direction === "incoming";
          const sender = () => incoming() ? peerName(transfer()) : localName();
          const recipient = () => incoming() ? localName() : peerName(transfer());
          const progressMax = () => Math.max(1, transfer().size);
          const progressValue = () => transfer().size === 0 && transfer().status === "completed" ? 1 : Math.min(transfer().transferred, progressMax());
          return (
            <article class="transfer-row" classList={{ failed: transfer().status === "failed" }}>
              <div class="transfer-main">
                <strong title={transfer().name}>{transfer().name}</strong>
                <span>{sender()} → {recipient()}</span>
              </div>
              <div class="transfer-progress">
                <progress value={progressValue()} max={progressMax()} aria-label={`${transfer().name} 传输进度`} />
                <span>{formatBytes(transfer().transferred)} / {formatBytes(transfer().size)}</span>
              </div>
              <div class="transfer-state">
                <b>{status(transfer())}</b>
                <Show when={transfer().error}><small title={transfer().error}>{transfer().error}</small></Show>
                <Show when={incoming() && transfer().status === "completed" && transfer().savedPath}>
                  <small title={transfer().savedPath}>{transfer().savedPath}</small>
                </Show>
                <Show when={transfer().sha256}><small class="transfer-hash" title={transfer().sha256}>SHA-256 {transfer().sha256.slice(0, 10)}…</small></Show>
              </div>
              <div class="transfer-actions">
                <Show when={incoming() && transfer().status === "offered"}>
                  <button type="button" disabled={props.busy} onClick={(event) => void runTransferAction(event.currentTarget, () => Backend.AcceptFile(transferID))}>接收</button>
                  <button type="button" disabled={props.busy} onClick={(event) => void runTransferAction(event.currentTarget, () => Backend.RejectFile(transferID))}>拒绝</button>
                </Show>
                <Show when={!terminal(transfer()) && !(incoming() && transfer().status === "offered")}>
                  <button type="button" disabled={props.busy} onClick={(event) => void runTransferAction(event.currentTarget, () => Backend.CancelFile(transferID))}>取消</button>
                </Show>
              </div>
            </article>
          );
        }}</For>
      </section>
    </Show>
  );
}

interface AudioControlProps {
  kind: "capture" | "playback";
  label: string;
  muted: boolean;
  gain: number;
  disabled: boolean;
  setMuted: (muted: boolean) => void;
  setGain: (gain: number) => void | Promise<unknown>;
}

function AudioControl(props: AudioControlProps) {
  const [draftGain, setDraftGain] = createSignal(props.gain);
  let editingGain = false;
  const actionLabel = () => props.muted ? `取消${props.label}静音` : `将${props.label}静音`;
  createEffect(() => {
    const gain = props.gain;
    if (!editingGain) setDraftGain(gain);
  });

  async function commitGain() {
    try {
      await props.setGain(draftGain());
    } finally {
      editingGain = false;
      setDraftGain(props.gain);
    }
  }

  return (
    <div class="audio-control">
      <button
        class="audio-icon-button"
        classList={{ muted: props.muted }}
        type="button"
        disabled={props.disabled}
        aria-label={actionLabel()}
        aria-pressed={props.muted}
        title={actionLabel()}
        onClick={() => props.setMuted(!props.muted)}
      >
        <Show when={props.kind === "capture"} fallback={<SpeakerIcon muted={props.muted} />}>
          <MicrophoneIcon muted={props.muted} />
        </Show>
      </button>
      <div class="gain-slider">
        <input
          type="range"
          min="0"
          max="200"
          step="5"
          value={draftGain()}
          disabled={props.disabled}
          aria-label={`${props.label}音量`}
          title={`${props.label}音量 ${draftGain()}%`}
          onInput={(event) => { editingGain = true; setDraftGain(event.currentTarget.valueAsNumber); }}
          onChange={() => void commitGain()}
        />
        <output>{draftGain()}%</output>
      </div>
    </div>
  );
}

function formatBytes(value: number): string {
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB`;
  if (value < 1024 * 1024 * 1024) return `${(value / 1024 / 1024).toFixed(1)} MiB`;
  return `${(value / 1024 / 1024 / 1024).toFixed(1)} GiB`;
}

function ScreenShareIcon() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true">
      <rect x="3" y="4" width="18" height="13" rx="2" />
      <path d="M8 21h8M12 17v4M9 10l3-3 3 3M12 7v6" />
    </svg>
  );
}

function FileShareIcon() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true">
      <path d="M6 3h8l4 4v14H6zM14 3v5h5M9 14h6M12 11v6" />
    </svg>
  );
}

function MicrophoneIcon(props: { muted: boolean }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true">
      <rect x="9" y="3" width="6" height="11" rx="3" />
      <path d="M6.5 11.5a5.5 5.5 0 0 0 11 0M12 17v4M9 21h6" />
      <Show when={props.muted}><path class="mute-slash" d="M4 4l16 16" /></Show>
    </svg>
  );
}

function SpeakerIcon(props: { muted: boolean }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true">
      <path d="M11 5 6.5 9H3v6h3.5l4.5 4z" />
      <Show when={!props.muted}>
        <path d="M15 9a4 4 0 0 1 0 6M17.5 6.5a7.5 7.5 0 0 1 0 11" />
      </Show>
      <Show when={props.muted}><path class="mute-slash" d="M4 4l16 16" /></Show>
    </svg>
  );
}
