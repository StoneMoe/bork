import { For, Show, createEffect, createMemo, createSignal, onCleanup } from "solid-js";
import * as Backend from "@wailsjs/go/app/App";
import type { screenshare } from "@wailsjs/go/models";
import { EventsOn } from "@wailsjs/runtime/runtime";
import { RoomControlRow, RoomMemberList } from "./RoomControls";
import { nativePopoverOpen, nativePopoverSupported } from "./popover";
import type { ActionProps, AppState, FriendlyStatus, PushToTalkPreference } from "./types";

const screenVideoCodecs = ["avc1.42E01F", "avc1.4D401F"] as const;
const maxScreenVideoChunkBytes = 256 * 1024;
const screenVideoCanvasOptions: CanvasRenderingContext2DSettings = { alpha: false, colorSpace: "srgb" };
// Native capture produces SDR H.264. Tell WebCodecs not to infer HDR metadata
// from the display where the frame is rendered.
const screenVideoColorSpace = {
  primaries: "bt709",
  transfer: "bt709",
  matrix: "bt709",
  fullRange: false,
} satisfies VideoColorSpaceInit;
const localScreenSharer = "local";
const defaultScreenAspectRatio = 16 / 9;
const screenViewportMargin = 8;
const screenStageDefaultInset = 24;
const screenStageMinWidth = 220;
const screenStageMinHeight = 124;
const screenResizeDirections = ["n", "ne", "e", "se", "s", "sw", "w", "nw"] as const;

type ScreenResizeDirection = typeof screenResizeDirections[number];

// Corner handles follow whichever pointer axis changes the size more. A side
// handle centers the other axis, then shifts the stage to stay in the viewport.
function resizedScreenWidth(direction: ScreenResizeDirection, bounds: DOMRect, deltaX: number, deltaY: number, aspectRatio: number) {
  const horizontalChange = (direction.includes("e") ? 1 : direction.includes("w") ? -1 : 0) * deltaX;
  const verticalChange = (direction.includes("s") ? 1 : direction.includes("n") ? -1 : 0) * deltaY * aspectRatio;
  return bounds.width + (Math.abs(verticalChange) > Math.abs(horizontalChange) ? verticalChange : horizontalChange);
}

function availableScreenAxisSize(start: number, end: number, viewportSize: number, growsBefore: boolean, growsAfter: boolean) {
  if (growsBefore) return end - screenViewportMargin;
  if (growsAfter) return viewportSize - screenViewportMargin - start;
  return viewportSize - screenViewportMargin * 2;
}

function resizedScreenAxisStart(start: number, end: number, size: number, viewportSize: number, growsBefore: boolean, growsAfter: boolean) {
  if (growsBefore) return end - size;
  if (growsAfter) return start;
  return Math.min(
    viewportSize - screenViewportMargin - size,
    Math.max(screenViewportMargin, (start + end - size) / 2),
  );
}

function resizedScreenStage(bounds: DOMRect, direction: ScreenResizeDirection, deltaX: number, deltaY: number, aspectRatio: number) {
  const growsWest = direction.includes("w");
  const growsEast = direction.includes("e");
  const growsNorth = direction.includes("n");
  const growsSouth = direction.includes("s");
  const maxWidth = Math.max(1, Math.min(
    availableScreenAxisSize(bounds.left, bounds.right, window.innerWidth, growsWest, growsEast),
    availableScreenAxisSize(bounds.top, bounds.bottom, window.innerHeight, growsNorth, growsSouth) * aspectRatio,
  ));
  const minWidth = Math.min(maxWidth, Math.max(screenStageMinWidth, screenStageMinHeight * aspectRatio));
  const width = Math.min(maxWidth, Math.max(minWidth, resizedScreenWidth(direction, bounds, deltaX, deltaY, aspectRatio)));
  const height = width / aspectRatio;
  return {
    left: resizedScreenAxisStart(bounds.left, bounds.right, width, window.innerWidth, growsWest, growsEast),
    top: resizedScreenAxisStart(bounds.top, bounds.bottom, height, window.innerHeight, growsNorth, growsSouth),
    width,
  };
}

interface RoomProps extends ActionProps {
  state: AppState;
  friendly: FriendlyStatus;
  pushToTalk: PushToTalkPreference;
  configurePushToTalk: (enabled: boolean, code: string) => Promise<boolean>;
  screenFullscreen: boolean;
  reportError: (message: string) => void;
  registerLeaveAction: (action: (() => Promise<void>) | undefined) => void;
  toggleScreenFullscreen: () => void;
  exitScreenFullscreen: () => void;
}

export default function Room(props: RoomProps) {
  const [captureBusy, setCaptureBusy] = createSignal(false);
  const [currentCaptureID, setCurrentCaptureID] = createSignal(0);
  const [screenSources, setScreenSources] = createSignal<screenshare.Source[]>([]);
  const [localVideoReady, setLocalVideoReady] = createSignal(false);
  const [selectedSharer, setSelectedSharer] = createSignal("");
  const [screenAspectRatio, setScreenAspectRatio] = createSignal(defaultScreenAspectRatio);
  const [remoteVideoReady, setRemoteVideoReady] = createSignal(false);
  const [remoteVideoRecovering, setRemoteVideoRecovering] = createSignal(false);
  let captureRun = 0;
  let sourceDialog: HTMLDialogElement | undefined;
  let localCanvas: HTMLCanvasElement | undefined;
  let localDecoder: VideoDecoder | undefined;
  let localNeedsKeyframe = true;
  let pendingEndedCaptureID = 0;
  let remoteCanvas: HTMLCanvasElement | undefined;
  let remoteDecoder: VideoDecoder | undefined;
  let remoteDecoderSetup: Promise<void> | undefined;
  let remoteVideoIdentity = "";
  let selectedStreamIdentity = "";
  let remoteVideoRun = 0;
  let remoteNeedsKeyframe = true;
  let remoteLastChunkID = 0;
  let screenAudioSourceUpdate = Promise.resolve();
  let screenStage: HTMLElement | undefined;
  const screenStageObserver = new ResizeObserver(() => {
    if (screenStage?.isConnected) clampScreenStage(screenStage);
  });
  screenStageObserver.observe(document.documentElement);
  const pendingScreenKeyframes = new Map<string, ScreenVideoChunkEvent>();
  let waitingRegion: HTMLDivElement | undefined;
  let roomPeersRegion: HTMLElement | undefined;

  const remotePeers = () => props.state.room?.remotePeers ?? [];
  // Snapshot objects change for unrelated room state. Keep the capture
  // transition tied to the screen-sharing bit itself.
  const backendScreenSharing = createMemo(() => Boolean(props.state.room?.screenSharing));
  const roomPeerID = createMemo(() => props.state.room?.peerId || "");
  const localScreenSharing = () => currentCaptureID() > 0 || backendScreenSharing();
  const monitorSources = () => screenSources().filter((source) => source.kind === "monitor");
  const windowSources = () => screenSources().filter((source) => source.kind === "window");
  const remoteSharers = () => remotePeers().filter((peer) => peer.screenSharing);
  const selectedLocalScreen = () => selectedSharer() === localScreenSharer;
  const screenSharerIDs = () => [
    ...(localScreenSharing() ? [localScreenSharer] : []),
    ...remoteSharers().map((peer) => peer.peerId),
  ];
  const selectedRemoteSharer = () => remoteSharers().find((candidate) => candidate.peerId === selectedSharer());
  const selectedSharerName = () => {
    if (selectedLocalScreen()) return "你";
    const peer = selectedRemoteSharer();
    return peer?.nickname || peer?.peerId.slice(0, 14) || "房间成员";
  };
  const selectedVideoReady = () => selectedLocalScreen() ? localVideoReady() : remoteVideoReady();
  const remoteVideoInterrupted = () => !selectedLocalScreen() && (remoteVideoRecovering() || selectedRemoteSharer()?.connected === false);
  let previousPeerCount = remotePeers().length;
  const keepScreenStageOnTop = (event: Event) => {
    if (!nativePopoverSupported || event.target === screenStage || (event as ToggleEvent).newState !== "open") return;
    queueMicrotask(() => {
      const stage = screenStage;
      if (!stage?.isConnected || !nativePopoverOpen(stage)) return;
      stage.hidePopover();
      stage.showPopover();
    });
  };
  document.addEventListener("beforetoggle", keepScreenStageOnTop, true);
  props.registerLeaveAction(leaveRoom);
  let previousLocalScreenSharing = backendScreenSharing();

  function focusRoomFallback() {
    queueMicrotask(() => {
      if (waitingRegion?.isConnected) waitingRegion.focus({ preventScroll: true });
      else if (roomPeersRegion?.isConnected) roomPeersRegion.focus({ preventScroll: true });
    });
  }

  const removeScreenListener = EventsOn("bork:screen-video-chunk", (chunk: ScreenVideoChunkEvent) => {
    if (!validScreenVideoChunkEvent(chunk)) return;
    const peer = remotePeers().find((candidate) => candidate.peerId === chunk.peerId && candidate.sessionId === chunk.sessionId);
    const currentStream = peer?.screenSharing && peer.screenGeneration === chunk.generation && peer.screenStreamId === chunk.streamId;
    if (!currentStream) {
      if (peer?.screenSharing && chunk.generation <= peer.screenGeneration) return;
      if (chunk.keyFrame) {
        if (pendingScreenKeyframes.size >= 64) pendingScreenKeyframes.delete(pendingScreenKeyframes.keys().next().value!);
        pendingScreenKeyframes.set(`${chunk.peerId}:${chunk.sessionId}`, chunk);
      }
      return;
    }
    if (!selectedSharer()) selectSharer(chunk.peerId);
    if (selectedSharer() === chunk.peerId) void receiveScreenVideoChunk(chunk);
  });
  const removeScreenPreviewListener = EventsOn("bork:screen-preview-chunk", (chunk: LocalScreenVideoChunkEvent) => {
    if (chunk?.captureId !== currentCaptureID() || !validEncodedScreenVideo(chunk)) return;
    if (!selectedSharer()) selectSharer(localScreenSharer);
    if (selectedLocalScreen()) receiveLocalScreenVideoChunk(chunk);
  });
  const removeScreenPreviewEndedListener = EventsOn("bork:screen-preview-ended", (captureID: number) => {
    if (!Number.isInteger(captureID) || captureID <= 0 || captureID > 0xffffffff) return;
    if (captureID === currentCaptureID()) {
      finishEndedScreenShare();
    } else {
      pendingEndedCaptureID = Math.max(pendingEndedCaptureID, captureID);
    }
  });

  createEffect(() => {
    const sharers = remoteSharers();
    const sharerIDs = screenSharerIDs();
    if (!sharerIDs.includes(selectedSharer())) selectSharer(sharerIDs[0] || "");
    const selected = sharers.find((peer) => peer.peerId === selectedSharer());
    const streamIdentity = selected ? `${selected.peerId}:${selected.sessionId}:${selected.screenGeneration}:${selected.screenStreamId}` : "";
    if (streamIdentity !== selectedStreamIdentity) {
      selectedStreamIdentity = streamIdentity;
      resetRemoteScreenVideo();
    }
    for (const [key, chunk] of pendingScreenKeyframes) {
      const current = remotePeers().find((peer) => peer.peerId === chunk.peerId);
      if (current && current.sessionId !== chunk.sessionId) {
        pendingScreenKeyframes.delete(key);
        continue;
      }
      if (!current?.screenSharing || current.screenGeneration !== chunk.generation || current.screenStreamId !== chunk.streamId) continue;
      pendingScreenKeyframes.delete(key);
      if (!selectedSharer()) selectSharer(chunk.peerId);
      if (selectedSharer() === chunk.peerId) void receiveScreenVideoChunk(chunk);
    }
  });

  createEffect(() => {
    const currentRoomPeerID = roomPeerID();
    const selected = selectedSharer();
    if (!currentRoomPeerID) return;
    // Local preview audio is already excluded from playback. An empty source
    // also silences screen audio while there is no selected share. Keep these
    // calls in selection order because Wails dispatches backend calls in parallel.
    screenAudioSourceUpdate = screenAudioSourceUpdate
      .then(() => Backend.SetScreenAudioSource(currentRoomPeerID, selected === localScreenSharer ? "" : selected))
      .catch(reportScreenError);
  });

  createEffect(() => {
    const peerCount = remotePeers().length;
    if (previousPeerCount > 0 && peerCount === 0) {
      if (captureBusy() || localScreenSharing()) void stopScreenShare(true);
      queueMicrotask(() => {
        const active = document.activeElement;
        if (!active || active === document.body || !active.isConnected) focusRoomFallback();
      });
    }
    previousPeerCount = peerCount;
  });

  createEffect(() => {
    const screenSharing = backendScreenSharing();
    const captureID = currentCaptureID();
    if (previousLocalScreenSharing && !screenSharing) {
      setCurrentCaptureID(0);
      resetLocalScreenVideo();
    }
    if (Boolean(captureID) === screenSharing) setCaptureBusy(false);
    previousLocalScreenSharing = screenSharing;
  });

  createEffect(() => {
    if (!localScreenSharing() && remoteSharers().length === 0) props.exitScreenFullscreen();
  });

  function selectSharer(peerID: string) {
    if (selectedSharer() === peerID) return;
    resetLocalScreenVideo();
    resetRemoteScreenVideo();
    setScreenAspectRatio(defaultScreenAspectRatio);
    setSelectedSharer(peerID);
  }

  function startScreenStageDrag(event: PointerEvent) {
    if (event.button !== 0 || !event.isPrimary || props.screenFullscreen) return;
    const stage = event.currentTarget as HTMLElement;
    const bounds = stage.getBoundingClientRect();
    stage.style.left = `${bounds.left}px`;
    stage.style.top = `${bounds.top}px`;
    stage.style.right = "auto";
    stage.style.bottom = "auto";
    trackScreenStagePointer(event, "dragging", (next) => {
      const maxLeft = Math.max(screenViewportMargin, window.innerWidth - stage.offsetWidth - screenViewportMargin);
      const maxTop = Math.max(screenViewportMargin, window.innerHeight - stage.offsetHeight - screenViewportMargin);
      const left = Math.min(maxLeft, Math.max(screenViewportMargin, bounds.left + next.clientX - event.clientX));
      const top = Math.min(maxTop, Math.max(screenViewportMargin, bounds.top + next.clientY - event.clientY));
      stage.style.left = `${left}px`;
      stage.style.top = `${top}px`;
    });
  }

  function startScreenStageResize(event: PointerEvent, direction: ScreenResizeDirection) {
    if (event.button !== 0 || !event.isPrimary || !screenStage || props.screenFullscreen) return;
    event.stopPropagation();
    const stage = screenStage;
    const bounds = stage.getBoundingClientRect();
    stage.style.left = `${bounds.left}px`;
    stage.style.top = `${bounds.top}px`;
    stage.style.right = "auto";
    stage.style.bottom = "auto";
    stage.style.width = `${bounds.width}px`;
    trackScreenStagePointer(event, "resizing", (next) => {
      const deltaX = next.clientX - event.clientX;
      const deltaY = next.clientY - event.clientY;
      const resized = resizedScreenStage(bounds, direction, deltaX, deltaY, screenAspectRatio());
      stage.style.left = `${resized.left}px`;
      stage.style.top = `${resized.top}px`;
      stage.style.width = `${resized.width}px`;
    });
  }

  function trackScreenStagePointer(event: PointerEvent, activeClass: string, move: (next: PointerEvent) => void) {
    const target = event.currentTarget as HTMLElement;
    event.preventDefault();
    target.setPointerCapture(event.pointerId);
    screenStage?.classList.add(activeClass);
    const handleMove = (next: PointerEvent) => {
      if (next.pointerId === event.pointerId) move(next);
    };
    const stop = (next: PointerEvent) => {
      if (next.pointerId !== event.pointerId) return;
      screenStage?.classList.remove(activeClass);
      target.removeEventListener("pointermove", handleMove);
      target.removeEventListener("pointerup", stop);
      target.removeEventListener("pointercancel", stop);
      target.removeEventListener("lostpointercapture", stop);
      if (next.type !== "lostpointercapture" && target.hasPointerCapture(event.pointerId)) target.releasePointerCapture(event.pointerId);
    };
    target.addEventListener("pointermove", handleMove);
    target.addEventListener("pointerup", stop);
    target.addEventListener("pointercancel", stop);
    target.addEventListener("lostpointercapture", stop);
  }

  function bindScreenStage(stage: HTMLElement) {
    if (screenStage) screenStageObserver.unobserve(screenStage);
    screenStage = stage;
    screenStageObserver.observe(stage);
    if (nativePopoverSupported) {
      queueMicrotask(() => {
        if (screenStage === stage && stage.isConnected && !nativePopoverOpen(stage)) stage.showPopover();
      });
    }
  }

  function clampScreenStage(stage: HTMLElement) {
    if (props.screenFullscreen) return;
    const aspectRatio = screenAspectRatio();
    const maxWidth = Math.max(1, Math.min(
      window.innerWidth - screenViewportMargin * 2,
      (window.innerHeight - screenViewportMargin * 2) * aspectRatio,
    ));
    const minWidth = Math.min(maxWidth, Math.max(screenStageMinWidth, screenStageMinHeight * aspectRatio));
    const currentWidth = stage.getBoundingClientRect().width;
    const width = Math.min(maxWidth, Math.max(minWidth, currentWidth));
    if (Math.abs(width - currentWidth) > 0.5) {
      stage.style.width = `${width}px`;
    }
    const defaultPosition = !stage.style.left && !stage.style.top;
    if (defaultPosition) {
      const right = stage.offsetWidth + screenStageDefaultInset <= window.innerWidth - screenViewportMargin
        ? screenStageDefaultInset : screenViewportMargin;
      const bottom = stage.offsetHeight + screenStageDefaultInset <= window.innerHeight - screenViewportMargin
        ? screenStageDefaultInset : screenViewportMargin;
      stage.style.right = `${right}px`;
      stage.style.bottom = `${bottom}px`;
      return;
    }
    const bounds = stage.getBoundingClientRect();
    const left = Math.min(
      Math.max(screenViewportMargin, window.innerWidth - stage.offsetWidth - screenViewportMargin),
      Math.max(screenViewportMargin, bounds.left),
    );
    const top = Math.min(
      Math.max(screenViewportMargin, window.innerHeight - stage.offsetHeight - screenViewportMargin),
      Math.max(screenViewportMargin, bounds.top),
    );
    stage.style.right = "auto";
    stage.style.bottom = "auto";
    stage.style.left = `${left}px`;
    stage.style.top = `${top}px`;
  }

  onCleanup(() => {
    props.registerLeaveAction(undefined);
    props.exitScreenFullscreen();
    screenStageObserver.disconnect();
    document.removeEventListener("beforetoggle", keepScreenStageOnTop, true);
    if (sourceDialog?.open) sourceDialog.close();
    removeScreenListener();
    removeScreenPreviewListener();
    removeScreenPreviewEndedListener();
    resetLocalScreenVideo();
    resetRemoteScreenVideo();
    void stopScreenShare(false);
  });

  function reportScreenError(cause: unknown) {
    const message = cause instanceof Error ? cause.message : String(cause || "屏幕分享失败");
    props.reportError(message.replace(/^Error:\s*/, ""));
  }

  async function openScreenSourcePicker() {
    if (captureBusy() || sourceDialog?.open) return;
    setCaptureBusy(true);
    const run = ++captureRun;
    try {
      const sources = await Backend.ListScreenSources();
      if (run !== captureRun) return;
      if (sources.length === 0) throw new Error("没有可分享的窗口或显示器");
      setScreenSources(sources);
      sourceDialog?.showModal();
    } catch (cause) {
      if (run === captureRun) reportScreenError(cause);
    } finally {
      if (run === captureRun && !sourceDialog?.open) setCaptureBusy(false);
    }
  }

  function closeScreenSourcePicker() {
    setScreenSources([]);
    setCaptureBusy(false);
    if (sourceDialog?.open) sourceDialog.close();
  }

  async function startScreenShare(sourceID: string) {
    if (!captureBusy() || !sourceDialog?.open) return;
    sourceDialog.close();
    focusRoomFallback();
    setScreenSources([]);
    resetLocalScreenVideo();
    const run = ++captureRun;
    let startedID = 0;
    try {
      startedID = await Backend.StartScreenShare(sourceID);
      if (!Number.isInteger(startedID) || startedID <= 0) throw new Error("后端没有创建屏幕捕获会话");
      if (run !== captureRun) {
        await Backend.StopScreenShare(startedID);
        return;
      }
      if (pendingEndedCaptureID === startedID) {
        finishEndedScreenShare();
        return;
      }
      pendingEndedCaptureID = 0;
      setCurrentCaptureID(startedID);
      selectSharer(localScreenSharer);
    } catch (cause) {
      if (startedID) {
        try { await Backend.StopScreenShare(startedID); } catch { /* stale cleanup */ }
      }
      if (run === captureRun) {
        clearLocalScreenShare();
        reportScreenError(cause);
      }
    }
  }

  async function stopScreenShare(reportBackendError: boolean) {
    const captureID = currentCaptureID();
    const backendShareActive = backendScreenSharing();
    ++captureRun;
    setCaptureBusy(true);
    clearLocalScreenShare(false);
    if (!captureID && !backendShareActive) {
      setCaptureBusy(false);
      return;
    }
    try {
      await Backend.StopScreenShare(captureID);
    } catch (cause) {
      if (captureID && pendingEndedCaptureID !== captureID) setCurrentCaptureID(captureID);
      setCaptureBusy(false);
      if (reportBackendError) reportScreenError(cause);
    }
  }

  function clearLocalScreenShare(finishTransition = true) {
    if (sourceDialog?.open) sourceDialog.close();
    setScreenSources([]);
    setCurrentCaptureID(0);
    pendingEndedCaptureID = 0;
    resetLocalScreenVideo();
    if (finishTransition) setCaptureBusy(false);
  }

  function finishEndedScreenShare() {
    clearLocalScreenShare(false);
    // Keep the control disabled until the coalesced backend snapshot also says
    // the share ended. This prevents a stale stop click during that short gap.
    setCaptureBusy(backendScreenSharing());
  }

  function receiveLocalScreenVideoChunk(chunk: LocalScreenVideoChunkEvent) {
    setScreenAspectRatio(chunk.width / chunk.height);
    if (localNeedsKeyframe && !chunk.keyFrame) return;
    try {
      let decoder = localDecoder;
      if (!decoder) {
        if (typeof VideoDecoder !== "function" || typeof EncodedVideoChunk !== "function") return;
        decoder = new VideoDecoder({
          output: (frame) => renderLocalVideoFrame(frame, chunk.captureId, chunk.width, chunk.height),
          error: () => resetLocalScreenVideo(false),
        });
        decoder.configure(screenVideoDecoderConfig(chunk));
        localDecoder = decoder;
      }
      if (decoder.state !== "configured") return;
      if (decoder.decodeQueueSize > 2) {
        resetLocalScreenVideo(false);
        return;
      }
      decoder.decode(encodedScreenVideoChunk(chunk));
      if (chunk.keyFrame) localNeedsKeyframe = false;
    } catch {
      resetLocalScreenVideo(false);
    }
  }

  function renderLocalVideoFrame(frame: VideoFrame, captureID: number, width: number, height: number) {
    try {
      if (captureID !== currentCaptureID() || !localCanvas) return;
      drawScreenVideoFrame(frame, localCanvas, width, height);
      setLocalVideoReady(true);
    } catch {
      resetLocalScreenVideo(false);
    } finally {
      frame.close();
    }
  }

  function resetLocalScreenVideo(clearFrame = true) {
    const decoder = localDecoder;
    localDecoder = undefined;
    if (decoder && decoder.state !== "closed") {
      try { decoder.close(); } catch { /* already failed */ }
    }
    localNeedsKeyframe = true;
    if (!clearFrame) return;
    setLocalVideoReady(false);
    const context = localCanvas?.getContext("2d");
    if (context && localCanvas) context.clearRect(0, 0, localCanvas.width, localCanvas.height);
  }

  async function receiveScreenVideoChunk(chunk: ScreenVideoChunkEvent) {
    setScreenAspectRatio(chunk.width / chunk.height);
    const identity = `${chunk.peerId}:${chunk.sessionId}:${chunk.generation}:${chunk.streamId}:${chunk.codec}:${chunk.width}x${chunk.height}`;
    if (identity !== remoteVideoIdentity) {
      resetRemoteScreenVideo();
      remoteVideoIdentity = identity;
    }
    if (remoteLastChunkID && chunk.chunkId !== remoteLastChunkID + 1) resetRemoteScreenVideo(false);
    remoteLastChunkID = chunk.chunkId;
    if (remoteNeedsKeyframe && !chunk.keyFrame) return;
    let encodedChunk: EncodedVideoChunk;
    try {
      encodedChunk = encodedScreenVideoChunk(chunk);
    } catch (cause) {
      reportScreenError(cause);
      resetRemoteScreenVideo(false);
      return;
    }
    const run = remoteVideoRun;
    try {
      await ensureRemoteVideoDecoder(chunk, identity, run);
      if (run !== remoteVideoRun || identity !== remoteVideoIdentity || selectedSharer() !== chunk.peerId) return;
      const decoder = remoteDecoder;
      if (!decoder || decoder.state !== "configured") return;
      if (decoder.decodeQueueSize > 2) {
        resetRemoteScreenVideo(false);
        return;
      }
      if (remoteNeedsKeyframe && !chunk.keyFrame) return;
      decoder.decode(encodedChunk);
      if (chunk.keyFrame) remoteNeedsKeyframe = false;
    } catch (cause) {
      if (run === remoteVideoRun) {
        reportScreenError(cause);
        resetRemoteScreenVideo(false);
      }
    }
  }

  async function ensureRemoteVideoDecoder(chunk: ScreenVideoChunkEvent, identity: string, run: number) {
    if (remoteDecoder?.state === "configured") return;
    if (!remoteDecoderSetup) {
      remoteDecoderSetup = (async () => {
        if (!window.isSecureContext) throw new Error("屏幕视频需要安全上下文 (HTTPS 或本机应用)");
        if (typeof VideoDecoder !== "function" || typeof EncodedVideoChunk !== "function") {
          throw new Error("当前系统暂不支持播放屏幕分享");
        }
        const config = screenVideoDecoderConfig(chunk);
        const support = await VideoDecoder.isConfigSupported(config);
        if (!support.supported) throw new Error("当前系统暂不支持播放此屏幕分享");
        if (run !== remoteVideoRun || identity !== remoteVideoIdentity) return;
        const decoder = new VideoDecoder({
          output: (frame) => renderRemoteVideoFrame(frame, chunk, identity, run),
          error: (cause) => {
            if (run !== remoteVideoRun || identity !== remoteVideoIdentity) return;
            reportScreenError(new Error(`屏幕视频解码失败: ${cause.message}`));
            resetRemoteScreenVideo(false);
          },
        });
        decoder.configure(config);
        if (run !== remoteVideoRun || identity !== remoteVideoIdentity) {
          decoder.close();
          return;
        }
        remoteDecoder = decoder;
      })();
    }
    const setup = remoteDecoderSetup;
    try {
      await setup;
    } finally {
      if (remoteDecoderSetup === setup) remoteDecoderSetup = undefined;
    }
  }

  function renderRemoteVideoFrame(frame: VideoFrame, chunk: ScreenVideoChunkEvent, identity: string, run: number) {
    try {
      if (run !== remoteVideoRun || identity !== remoteVideoIdentity || selectedSharer() !== chunk.peerId) return;
      const canvas = remoteCanvas;
      if (!canvas) throw new Error("当前 WebView 无法渲染屏幕视频");
      drawScreenVideoFrame(frame, canvas, chunk.width, chunk.height);
      setRemoteVideoReady(true);
      setRemoteVideoRecovering(false);
    } catch (cause) {
      if (run === remoteVideoRun) {
        reportScreenError(cause);
        resetRemoteScreenVideo(false);
      }
    } finally {
      frame.close();
    }
  }

  function resetRemoteScreenVideo(clearIdentity = true) {
    const preserveFrame = !clearIdentity && remoteVideoReady();
    ++remoteVideoRun;
    remoteDecoderSetup = undefined;
    const decoder = remoteDecoder;
    remoteDecoder = undefined;
    if (decoder) {
      if (decoder.state === "configured") {
        try { decoder.reset(); } catch { /* already failed */ }
      }
      if (decoder.state !== "closed") {
        try { decoder.close(); } catch { /* already failed */ }
      }
    }
    if (clearIdentity) remoteVideoIdentity = "";
    remoteNeedsKeyframe = true;
    remoteLastChunkID = 0;
    setRemoteVideoRecovering(preserveFrame);
    if (!preserveFrame) {
      setRemoteVideoReady(false);
      const context = remoteCanvas?.getContext("2d");
      if (context && remoteCanvas) context.clearRect(0, 0, remoteCanvas.width, remoteCanvas.height);
    }
  }

  async function leaveRoom() {
    props.exitScreenFullscreen();
    await stopScreenShare(false);
    await props.runAction(Backend.LeaveRoom);
  }

  return (
    <section class="room-view">
      <dialog
        ref={sourceDialog}
        class="floating-card screen-source-card"
        aria-modal="true"
        aria-labelledby="screen-source-title"
        aria-describedby="screen-source-description"
        onCancel={(event) => {
          event.preventDefault();
          closeScreenSourcePicker();
        }}
      >
        <header>
          <div>
            <strong id="screen-source-title">选择分享内容</strong>
            <small id="screen-source-description">选择显示器或窗口；系统支持时，同时共享 Bork 之外的系统声音。</small>
          </div>
          <button type="button" onClick={closeScreenSourcePicker}>关闭</button>
        </header>
        <div class="screen-source-groups">
          <ScreenSourceGroup title="显示器" sources={monitorSources()} select={startScreenShare} />
          <ScreenSourceGroup title="窗口" sources={windowSources()} select={startScreenShare} />
        </div>
      </dialog>
      <div class="room-content">
        <section class="voice-stage">
          <section ref={roomPeersRegion} class="room-peers" aria-label="房间成员" tabindex="-1">
            <div class="room-member-area">
              <RoomMemberList state={props.state} remotePeers={remotePeers()} focusFallback={focusRoomFallback} />
              <Show when={remotePeers().length === 0}>
                <div ref={waitingRegion} class="voice-caption waiting-state" role="status" tabindex="-1">
                  <strong>{props.friendly.title}</strong>
                  <p>{props.friendly.detail}</p>
                </div>
              </Show>
            </div>
            <Show when={props.state.audio.error}>
              <p class="voice-error">{props.state.audio.error}</p>
            </Show>
            <Show when={!props.state.audio.available && !props.state.audio.error}>
              <p class="voice-error">没有可用的麦克风或扬声器。</p>
            </Show>
            <RoomControlRow
              state={props.state}
              remotePeers={remotePeers()}
              screenSharing={localScreenSharing()}
              captureBusy={captureBusy()}
              remoteSharerCount={remoteSharers().length}
              busy={props.busy}
              ready={props.ready}
              runAction={props.runAction}
              pushToTalk={props.pushToTalk}
              configurePushToTalk={props.configurePushToTalk}
              focusFallback={focusRoomFallback}
              toggleScreenShare={() => localScreenSharing() ? void stopScreenShare(true) : void openScreenSourcePicker()}
            />
          </section>
          <Show when={localScreenSharing() || remoteSharers().length > 0}>
            <section
              class="screen-stage"
              classList={{ fullscreen: props.screenFullscreen }}
              style={{ "aspect-ratio": `${screenAspectRatio()}` }}
              aria-label="屏幕分享画面"
              popover={nativePopoverSupported ? "manual" : undefined}
              ref={bindScreenStage}
              onPointerDown={startScreenStageDrag}
            >
              <button
                type="button"
                class="screen-fullscreen-toggle"
                aria-label={props.screenFullscreen ? "退出全屏" : "全屏显示"}
                title={props.screenFullscreen ? "退出全屏" : "全屏显示"}
                onPointerDown={(event) => event.stopPropagation()}
                onClick={props.toggleScreenFullscreen}
              >
                <svg viewBox="0 0 24 24" aria-hidden="true">
                  <Show
                    when={props.screenFullscreen}
                    fallback={<path d="M9 4H4v5M15 20h5v-5" />}
                  >
                    <path d="M4 9h5V4M20 15h-5v5" />
                  </Show>
                </svg>
              </button>
              <figure class="screen-preview">
                <Show
                  when={selectedLocalScreen()}
                  fallback={
                    <canvas
                      ref={(element) => { remoteCanvas = element; }}
                      classList={{ ready: remoteVideoReady() }}
                      role="img"
                      aria-label={`${selectedSharerName()}分享的屏幕`}
                    />
                  }
                >
                  <canvas
                    ref={(element) => { localCanvas = element; }}
                    classList={{ ready: localVideoReady() }}
                    role="img"
                    aria-label="你分享的屏幕"
                  />
                </Show>
                <Show when={!selectedVideoReady() || remoteVideoInterrupted()}>
                  <p class="screen-video-status" role="status">
                    {selectedLocalScreen()
                      ? (typeof VideoDecoder === "function" ? "正在准备本机预览…" : "画面正在共享")
                      : (remoteVideoInterrupted() ? "画面中断，正在恢复…" : "正在接收画面…")}
                  </p>
                </Show>
                <figcaption class="screen-sharer">
                  <Show when={screenSharerIDs().length > 1} fallback={<span>{selectedSharerName()}</span>}>
                    <select
                      class="screen-sharer-select"
                      aria-label="选择要观看的屏幕分享者"
                      value={selectedSharer()}
                      onPointerDown={(event) => event.stopPropagation()}
                      onChange={(event) => selectSharer(event.currentTarget.value)}
                    >
                      <Show when={localScreenSharing()}>
                        <option value={localScreenSharer}>你</option>
                      </Show>
                      <For each={remoteSharers().map((peer) => peer.peerId)}>{(peerID) => {
                        const peer = () => remoteSharers().find((candidate) => candidate.peerId === peerID)!;
                        return <option value={peerID}>{peer().nickname || peerID.slice(0, 14)}</option>;
                      }}</For>
                    </select>
                  </Show>
                </figcaption>
              </figure>
              <For each={screenResizeDirections}>{(direction) => (
                <span
                  class="screen-resize-handle"
                  data-direction={direction}
                  aria-hidden="true"
                  onPointerDown={(event) => startScreenStageResize(event, direction)}
                />
              )}</For>
            </section>
          </Show>
        </section>
      </div>
    </section>
  );
}

function ScreenSourceGroup(props: {
  title: string;
  sources: screenshare.Source[];
  select: (sourceID: string) => Promise<void>;
}) {
  return (
    <Show when={props.sources.length > 0}>
      <section class="screen-source-group" aria-label={props.title}>
        <h3>{props.title}</h3>
        <div class="screen-source-list">
          <For each={props.sources}>{(source) => (
            <button type="button" onClick={() => void props.select(source.id)}>
              <strong>{source.name}</strong>
              <small>{source.width} × {source.height}</small>
            </button>
          )}</For>
        </div>
      </section>
    </Show>
  );
}

interface LocalScreenVideoChunkEvent extends EncodedScreenVideo {
  captureId: number;
}

interface ScreenVideoChunkEvent extends EncodedScreenVideo {
  peerId: string;
  sessionId: string;
  generation: number;
  streamId: string;
  chunkId: number;
}

function screenVideoDecoderConfig(chunk: EncodedScreenVideo): VideoDecoderConfig {
  return {
    codec: chunk.codec,
    codedWidth: chunk.width,
    codedHeight: chunk.height,
    colorSpace: screenVideoColorSpace,
    optimizeForLatency: true,
  };
}

function encodedScreenVideoChunk(chunk: EncodedScreenVideo): EncodedVideoChunk {
  return new EncodedVideoChunk({
    type: chunk.keyFrame ? "key" : "delta",
    timestamp: chunk.timestamp,
    duration: chunk.duration,
    data: base64Bytes(chunk.bytes),
  });
}

function drawScreenVideoFrame(frame: VideoFrame, canvas: HTMLCanvasElement, width: number, height: number) {
  if (frame.displayWidth <= 0 || frame.displayHeight <= 0 || frame.displayWidth > 1280 || frame.displayHeight > 720) {
    throw new Error("屏幕视频帧尺寸无效");
  }
  const context = canvas.getContext("2d", screenVideoCanvasOptions);
  if (!context) throw new Error("当前 WebView 无法渲染屏幕视频");
  if (canvas.width !== width || canvas.height !== height) {
    canvas.width = width;
    canvas.height = height;
  }
  context.drawImage(frame, 0, 0, width, height);
}

function validScreenVideoChunkEvent(value: unknown): value is ScreenVideoChunkEvent {
  if (!value || typeof value !== "object") return false;
  const chunk = value as Partial<ScreenVideoChunkEvent>;
  return typeof chunk.peerId === "string" && chunk.peerId.length > 0
    && typeof chunk.sessionId === "string" && /^[0-9a-f]{32}$/.test(chunk.sessionId)
    && Number.isSafeInteger(chunk.generation) && Number(chunk.generation) > 0
    && typeof chunk.streamId === "string" && /^[0-9a-f]{32}$/.test(chunk.streamId)
    && Number.isInteger(chunk.chunkId) && Number(chunk.chunkId) > 0 && Number(chunk.chunkId) <= 0xffffffff
    && validEncodedScreenVideo(chunk);
}

interface EncodedScreenVideo {
  codec: string;
  width: number;
  height: number;
  timestamp: number;
  duration: number;
  keyFrame: boolean;
  bytes: string;
}

function validEncodedScreenVideo(chunk: Partial<EncodedScreenVideo>): chunk is EncodedScreenVideo {
  return typeof chunk.codec === "string" && (screenVideoCodecs as readonly string[]).includes(chunk.codec)
    && Number.isInteger(chunk.width) && Number(chunk.width) >= 2 && Number(chunk.width) <= 1280 && Number(chunk.width) % 2 === 0
    && Number.isInteger(chunk.height) && Number(chunk.height) >= 2 && Number(chunk.height) <= 720 && Number(chunk.height) % 2 === 0
    && Number.isSafeInteger(chunk.timestamp) && Number(chunk.timestamp) >= 0
    && Number.isInteger(chunk.duration) && Number(chunk.duration) > 0 && Number(chunk.duration) <= 1_000_000
    && Number(chunk.timestamp) + Number(chunk.duration) <= Number.MAX_SAFE_INTEGER
    && typeof chunk.keyFrame === "boolean"
    && typeof chunk.bytes === "string" && chunk.bytes.length > 0 && chunk.bytes.length <= Math.ceil(maxScreenVideoChunkBytes / 3) * 4;
}

function base64Bytes(value: string): Uint8Array {
  let binary: string;
  try {
    binary = atob(value);
  } catch {
    throw new Error("屏幕视频块不是有效的 base64");
  }
  if (binary.length === 0 || binary.length > maxScreenVideoChunkBytes) throw new Error("屏幕视频块大小无效");
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index++) bytes[index] = binary.charCodeAt(index);
  return bytes;
}
