import { For, Show, createEffect, createSignal, onCleanup } from "solid-js";
import * as Backend from "@wailsjs/go/app/App";
import { EventsOn } from "@wailsjs/runtime/runtime";
import { RoomControlRow, RoomMemberList } from "./RoomControls";
import type { ActionProps, AppState, FriendlyStatus, RemotePeer } from "./types";

const screenVideoCodecs = ["avc1.42E01F", "avc1.4D401F"] as const;
const screenVideoFrameRate = 15;
const screenVideoFrameDuration = Math.round(1_000_000 / screenVideoFrameRate);
const maxScreenVideoChunkBytes = 256 * 1024;

interface RoomProps extends ActionProps {
  state: AppState;
  friendly: FriendlyStatus;
  reportError: (message: string) => void;
  registerLeaveAction: (action: (() => Promise<void>) | undefined) => void;
}

export default function Room(props: RoomProps) {
  const [captureBusy, setCaptureBusy] = createSignal(false);
  const [localStream, setLocalStream] = createSignal<MediaStream>();
  const [selectedSharer, setSelectedSharer] = createSignal("");
  const [remoteVideoReady, setRemoteVideoReady] = createSignal(false);
  const captureVideo = document.createElement("video");
  const captureCanvas = document.createElement("canvas");
  let displayStream: MediaStream | undefined;
  let videoEncoder: VideoEncoder | undefined;
  let currentCaptureID = 0;
  let captureRun = 0;
  let captureTimer: number | undefined;
  let captureTimestamp = 0;
  let lastKeyframeTimestamp = Number.NEGATIVE_INFINITY;
  let forceKeyframe = true;
  let screenChunkSending = false;
  let remoteCanvas: HTMLCanvasElement | undefined;
  let remoteDecoder: VideoDecoder | undefined;
  let remoteDecoderSetup: Promise<void> | undefined;
  let remoteVideoIdentity = "";
  let selectedStreamIdentity = "";
  let remoteVideoRun = 0;
  let remoteNeedsKeyframe = true;
  let remoteLastChunkID = 0;
  const pendingScreenKeyframes = new Map<string, ScreenVideoChunkEvent>();
  let waitingRegion: HTMLDivElement | undefined;
  let roomPeersRegion: HTMLElement | undefined;

  const remotePeers = () => props.state.room?.remotePeers ?? [];
  const remoteSharers = () => remotePeers().filter((peer) => peer.screenSharing);
  const remoteName = (peer: RemotePeer) => peer.nickname || peer.peerId.slice(0, 14);
  let previousPeerCount = remotePeers().length;
  props.registerLeaveAction(leaveRoom);

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

  createEffect(() => {
    const sharers = remoteSharers();
    if (!sharers.some((peer) => peer.peerId === selectedSharer())) selectSharer(sharers[0]?.peerId || "");
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
    const peerCount = remotePeers().length;
    if (previousPeerCount > 0 && peerCount === 0) {
      if (captureBusy() || displayStream || currentCaptureID || props.state.room?.screenSharing) void stopScreenShare(true);
      queueMicrotask(() => {
        const active = document.activeElement;
        if (!active || active === document.body || !active.isConnected) focusRoomFallback();
      });
    }
    previousPeerCount = peerCount;
  });

  function selectSharer(peerID: string) {
    if (selectedSharer() === peerID) return;
    resetRemoteScreenVideo();
    setSelectedSharer(peerID);
  }

  onCleanup(() => {
    props.registerLeaveAction(undefined);
    removeScreenListener();
    resetRemoteScreenVideo();
    void stopScreenShare(false);
  });

  function reportScreenError(cause: unknown) {
    const message = cause instanceof Error ? cause.message : String(cause || "屏幕分享失败");
    props.reportError(message.replace(/^Error:\s*/, ""));
  }

  async function startScreenShare() {
    if (captureBusy() || displayStream) return;
    try {
      requireScreenVideoAPIs(true);
    } catch (cause) {
      reportScreenError(cause);
      return;
    }
    const getDisplayMedia = navigator.mediaDevices?.getDisplayMedia;
    if (typeof getDisplayMedia !== "function") {
      reportScreenError(new Error("当前系统 WebView 不支持屏幕捕获"));
      return;
    }
    setCaptureBusy(true);
    const run = ++captureRun;
    let startedID = 0;
    try {
      const stream = await getDisplayMedia.call(navigator.mediaDevices, {
        video: { width: { max: 1280 }, height: { max: 720 }, frameRate: { ideal: screenVideoFrameRate, max: screenVideoFrameRate } },
        audio: false,
      });
      if (run !== captureRun) {
        stopTracks(stream);
        return;
      }
      const track = stream.getVideoTracks()[0];
      if (!track) throw new Error("屏幕捕获没有返回视频轨道");
      displayStream = stream;
      setLocalStream(stream);
      track.addEventListener("ended", () => {
        if (displayStream === stream) void stopScreenShare(true);
      }, { once: true });
      captureVideo.muted = true;
      captureVideo.playsInline = true;
      captureVideo.srcObject = stream;
      await captureVideo.play();
      if (run !== captureRun || displayStream !== stream) return;
      const dimensions = screenVideoDimensions(captureVideo.videoWidth, captureVideo.videoHeight);
      const config = await supportedScreenVideoConfig(dimensions.width, dimensions.height);
      if (run !== captureRun || displayStream !== stream) return;
      captureCanvas.width = dimensions.width;
      captureCanvas.height = dimensions.height;
      if (!captureCanvas.getContext("2d", { alpha: false })) throw new Error("当前 WebView 无法创建屏幕视频画布");
      const encoder = new VideoEncoder({
        output: (chunk) => sendEncodedScreenVideoChunk(run, chunk),
        error: (cause) => failLocalScreenVideo(run, cause),
      });
      videoEncoder = encoder;
      encoder.configure(config);
      startedID = await Backend.StartScreenShare(config.codec, dimensions.width, dimensions.height);
      if (!Number.isInteger(startedID) || startedID <= 0) throw new Error("后端没有创建屏幕捕获会话");
      if (run !== captureRun || displayStream !== stream) {
        await Backend.StopScreenShare(startedID);
        return;
      }
      currentCaptureID = startedID;
      captureTimestamp = 0;
      lastKeyframeTimestamp = Number.NEGATIVE_INFINITY;
      forceKeyframe = true;
      setCaptureBusy(false);
      captureScreenVideoFrame(run, startedID);
    } catch (cause) {
      if (startedID) {
        try { await Backend.StopScreenShare(startedID); } catch { /* stale cleanup */ }
      }
      if (run === captureRun) {
        clearLocalCapture();
        reportScreenError(cause);
      }
    }
  }

  async function stopScreenShare(reportBackendError: boolean) {
    const captureID = currentCaptureID;
    const backendShareActive = Boolean(props.state.room?.screenSharing);
    ++captureRun;
    clearLocalCapture();
    if (!captureID && !backendShareActive) return;
    try {
      await Backend.StopScreenShare(captureID);
    } catch (cause) {
      if (captureID) currentCaptureID = captureID;
      if (reportBackendError) reportScreenError(cause);
    }
  }

  function clearLocalCapture() {
    window.clearTimeout(captureTimer);
    captureTimer = undefined;
    currentCaptureID = 0;
    const encoder = videoEncoder;
    videoEncoder = undefined;
    if (encoder && encoder.state !== "closed") {
      try { encoder.close(); } catch { /* already failed */ }
    }
    screenChunkSending = false;
    captureTimestamp = 0;
    lastKeyframeTimestamp = Number.NEGATIVE_INFINITY;
    forceKeyframe = true;
    captureVideo.pause();
    captureVideo.srcObject = null;
    captureCanvas.width = 0;
    captureCanvas.height = 0;
    stopTracks(displayStream);
    displayStream = undefined;
    setLocalStream(undefined);
    setCaptureBusy(false);
  }

  function captureScreenVideoFrame(run: number, captureID: number) {
    const startedAt = performance.now();
    try {
      if (run !== captureRun || captureID !== currentCaptureID || !displayStream) return;
      const timestamp = captureTimestamp;
      captureTimestamp += screenVideoFrameDuration;
      const encoder = videoEncoder;
      if (encoder?.state === "configured" && encoder.encodeQueueSize === 0 && !screenChunkSending) {
        const context = captureCanvas.getContext("2d", { alpha: false });
        if (!context) throw new Error("屏幕视频画布不可用");
        context.drawImage(captureVideo, 0, 0, captureCanvas.width, captureCanvas.height);
        const keyFrame = forceKeyframe || timestamp-lastKeyframeTimestamp >= 2_000_000;
        const frame = new VideoFrame(captureCanvas, { timestamp, duration: screenVideoFrameDuration });
        try {
          encoder.encode(frame, { keyFrame });
          if (keyFrame) {
            forceKeyframe = false;
            lastKeyframeTimestamp = timestamp;
          }
        } finally {
          frame.close();
        }
      }
    } catch (cause) {
      failLocalScreenVideo(run, cause);
      return;
    }
    captureTimer = window.setTimeout(
      () => captureScreenVideoFrame(run, captureID),
      Math.max(0, 1000 / screenVideoFrameRate - (performance.now() - startedAt)),
    );
  }

  function sendEncodedScreenVideoChunk(run: number, chunk: EncodedVideoChunk) {
    if (run !== captureRun || !currentCaptureID) return;
    if (chunk.byteLength <= 0 || chunk.byteLength > maxScreenVideoChunkBytes) {
      failLocalScreenVideo(run, new Error(`屏幕视频块超过 ${maxScreenVideoChunkBytes / 1024} KiB 限制`));
      return;
    }
    if (screenChunkSending) {
      forceKeyframe = true;
      return;
    }
    const duration = chunk.duration ?? screenVideoFrameDuration;
    if (!Number.isSafeInteger(chunk.timestamp) || chunk.timestamp < 0 || !Number.isInteger(duration) || duration <= 0 || duration > 1_000_000) {
      failLocalScreenVideo(run, new Error("屏幕视频编码器返回了无效时间戳"));
      return;
    }
    const bytes = new Uint8Array(chunk.byteLength);
    chunk.copyTo(bytes);
    screenChunkSending = true;
    void Backend.SendScreenVideoChunk(currentCaptureID, chunk.timestamp, duration, chunk.type === "key", bytesBase64(bytes))
      .then((sent) => { if (!sent) forceKeyframe = true; })
      .catch((cause) => failLocalScreenVideo(run, cause))
      .finally(() => {
        if (run === captureRun) screenChunkSending = false;
      });
  }

  function failLocalScreenVideo(run: number, cause: unknown) {
    if (run !== captureRun) return;
    reportScreenError(cause);
    void stopScreenShare(false);
  }

  async function receiveScreenVideoChunk(chunk: ScreenVideoChunkEvent) {
    const identity = `${chunk.peerId}:${chunk.sessionId}:${chunk.generation}:${chunk.streamId}:${chunk.codec}:${chunk.width}x${chunk.height}`;
    if (identity !== remoteVideoIdentity) {
      resetRemoteScreenVideo();
      remoteVideoIdentity = identity;
    }
    if (remoteLastChunkID && chunk.chunkId !== remoteLastChunkID + 1) resetRemoteScreenVideo(false);
    remoteLastChunkID = chunk.chunkId;
    if (remoteNeedsKeyframe && !chunk.keyFrame) return;
    let bytes: Uint8Array;
    try {
      bytes = base64Bytes(chunk.bytes);
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
      decoder.decode(new EncodedVideoChunk({
        type: chunk.keyFrame ? "key" : "delta",
        timestamp: chunk.timestamp,
        duration: chunk.duration,
        data: bytes,
      }));
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
        requireScreenVideoAPIs(false);
        const config: VideoDecoderConfig = {
          codec: chunk.codec,
          codedWidth: chunk.width,
          codedHeight: chunk.height,
          optimizeForLatency: true,
        };
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
      if (frame.displayWidth <= 0 || frame.displayHeight <= 0 || frame.displayWidth > 1280 || frame.displayHeight > 720) {
        throw new Error("远端屏幕视频帧尺寸无效");
      }
      const canvas = remoteCanvas;
      const context = canvas?.getContext("2d", { alpha: false });
      if (!canvas || !context) throw new Error("当前 WebView 无法渲染屏幕视频");
      if (canvas.width !== chunk.width || canvas.height !== chunk.height) {
        canvas.width = chunk.width;
        canvas.height = chunk.height;
      }
      context.drawImage(frame, 0, 0, chunk.width, chunk.height);
      setRemoteVideoReady(true);
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
    setRemoteVideoReady(false);
    const context = remoteCanvas?.getContext("2d");
    if (context && remoteCanvas) context.clearRect(0, 0, remoteCanvas.width, remoteCanvas.height);
  }

  async function leaveRoom() {
    await stopScreenShare(false);
    await props.runAction(Backend.LeaveRoom);
  }

  return (
    <section class="room-view">
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
              screenSharing={Boolean(localStream()) || Boolean(props.state.room?.screenSharing)}
              captureBusy={captureBusy()}
              remoteSharerCount={remoteSharers().length}
              busy={props.busy}
              ready={props.ready}
              runAction={props.runAction}
              focusFallback={focusRoomFallback}
              toggleScreenShare={() => localStream() || props.state.room?.screenSharing ? void stopScreenShare(true) : void startScreenShare()}
            />
          </section>
          <Show when={localStream() || remoteSharers().length > 0}>
            <section
              class="screen-stage"
              classList={{ single: Number(Boolean(localStream())) + Number(remoteSharers().length > 0) === 1 }}
              aria-label="屏幕分享画面"
            >
              <Show when={localStream()}>{(stream) => (
                <figure class="screen-preview local-screen">
                  <figcaption><span>本机预览</span><b>正在分享</b></figcaption>
                  <video
                    muted
                    autoplay
                    playsinline
                    ref={(element) => { element.srcObject = stream(); }}
                  />
                </figure>
              )}</Show>
              <Show when={remoteSharers().length > 0}>
                <figure class="screen-preview remote-screen">
                  <figcaption>
                    <span>远端画面</span>
                    <Show
                      when={remoteSharers().length > 1}
                      fallback={<b>{remoteName(remoteSharers()[0])}</b>}
                    >
                      <select
                        aria-label="选择远端屏幕"
                        value={selectedSharer()}
                        onChange={(event) => selectSharer(event.currentTarget.value)}
                      >
                        <For each={remoteSharers().map((peer) => peer.peerId)}>{(peerID) => {
                          const peer = () => remoteSharers().find((candidate) => candidate.peerId === peerID)!;
                          return <option value={peerID}>{remoteName(peer())}</option>;
                        }}</For>
                      </select>
                    </Show>
                  </figcaption>
                  <canvas
                    ref={(element) => { remoteCanvas = element; }}
                    classList={{ ready: remoteVideoReady() }}
                    role="img"
                    aria-label="所选成员分享的屏幕"
                  />
                  <Show when={!remoteVideoReady()}>
                    <div class="screen-waiting">等待关键帧…</div>
                  </Show>
                </figure>
              </Show>
            </section>
          </Show>
        </section>
      </div>
    </section>
  );
}

interface ScreenVideoChunkEvent {
  peerId: string;
  sessionId: string;
  generation: number;
  streamId: string;
  chunkId: number;
  codec: string;
  width: number;
  height: number;
  timestamp: number;
  duration: number;
  keyFrame: boolean;
  bytes: string;
}

function stopTracks(stream?: MediaStream) {
  stream?.getTracks().forEach((track) => track.stop());
}

function requireScreenVideoAPIs(encoder: boolean) {
  if (!window.isSecureContext) throw new Error("屏幕视频需要安全上下文 (HTTPS 或本机应用)");
  if (encoder) {
    if (typeof VideoEncoder !== "function" || typeof VideoFrame !== "function") throw new Error("当前系统暂不支持屏幕分享");
  } else if (typeof VideoDecoder !== "function" || typeof EncodedVideoChunk !== "function") {
    throw new Error("当前系统暂不支持播放屏幕分享");
  }
}

function screenVideoDimensions(sourceWidth: number, sourceHeight: number) {
  if (!Number.isFinite(sourceWidth) || !Number.isFinite(sourceHeight) || sourceWidth <= 0 || sourceHeight <= 0) {
    throw new Error("屏幕捕获尚未产生视频画面");
  }
  const scale = Math.min(1, 1280 / sourceWidth, 720 / sourceHeight);
  return {
    width: Math.max(2, Math.floor(sourceWidth * scale / 2) * 2),
    height: Math.max(2, Math.floor(sourceHeight * scale / 2) * 2),
  };
}

async function supportedScreenVideoConfig(width: number, height: number): Promise<VideoEncoderConfig> {
  for (const codec of screenVideoCodecs) {
    const config: VideoEncoderConfig = {
      codec,
      width,
      height,
      bitrate: 1_000_000,
      framerate: screenVideoFrameRate,
      latencyMode: "realtime",
      avc: { format: "annexb" },
    };
    try {
      const support = await VideoEncoder.isConfigSupported(config);
      if (support.supported) return config;
    } catch { /* try the other canonical H.264 profile */ }
  }
  throw new Error("当前系统暂不支持分享此屏幕");
}

function validScreenVideoChunkEvent(value: unknown): value is ScreenVideoChunkEvent {
  if (!value || typeof value !== "object") return false;
  const chunk = value as Partial<ScreenVideoChunkEvent>;
  return typeof chunk.peerId === "string" && chunk.peerId.length > 0
    && typeof chunk.sessionId === "string" && /^[0-9a-f]{32}$/.test(chunk.sessionId)
    && Number.isSafeInteger(chunk.generation) && Number(chunk.generation) > 0
    && typeof chunk.streamId === "string" && /^[0-9a-f]{32}$/.test(chunk.streamId)
    && Number.isInteger(chunk.chunkId) && Number(chunk.chunkId) > 0 && Number(chunk.chunkId) <= 0xffffffff
    && typeof chunk.codec === "string" && (screenVideoCodecs as readonly string[]).includes(chunk.codec)
    && Number.isInteger(chunk.width) && Number(chunk.width) >= 2 && Number(chunk.width) <= 1280 && Number(chunk.width) % 2 === 0
    && Number.isInteger(chunk.height) && Number(chunk.height) >= 2 && Number(chunk.height) <= 720 && Number(chunk.height) % 2 === 0
    && Number.isSafeInteger(chunk.timestamp) && Number(chunk.timestamp) >= 0
    && Number.isInteger(chunk.duration) && Number(chunk.duration) > 0 && Number(chunk.duration) <= 1_000_000
    && Number(chunk.timestamp) + Number(chunk.duration) <= Number.MAX_SAFE_INTEGER
    && typeof chunk.keyFrame === "boolean"
    && typeof chunk.bytes === "string" && chunk.bytes.length > 0 && chunk.bytes.length <= Math.ceil(maxScreenVideoChunkBytes / 3) * 4;
}

function bytesBase64(bytes: Uint8Array): string {
  let binary = "";
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  }
  return btoa(binary);
}

function base64Bytes(value: string): Uint8Array {
  let binary: string;
  try {
    binary = atob(value);
  } catch {
    throw new Error("远端屏幕视频块不是有效的 base64");
  }
  if (binary.length === 0 || binary.length > maxScreenVideoChunkBytes) throw new Error("远端屏幕视频块大小无效");
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index++) bytes[index] = binary.charCodeAt(index);
  return bytes;
}
