import { createRoot } from "solid-js";
import { beforeEach, expect, it, vi } from "vitest";
import { app } from "@wailsjs/go/models";

const bridge = vi.hoisted(() => ({
  getSnapshot: vi.fn<() => Promise<app.AppSnapshot>>(),
  listener: undefined as (() => void) | undefined,
  remove: vi.fn(),
}));

vi.mock("@wailsjs/go/app/App", () => ({
  GetSnapshot: bridge.getSnapshot,
}));

vi.mock("@wailsjs/runtime/runtime", () => ({
  EventsOn: vi.fn((_name: string, listener: () => void) => {
    bridge.listener = listener;
    return bridge.remove;
  }),
}));

import { createRemoteState } from "./sync";

beforeEach(() => {
  bridge.getSnapshot.mockReset();
  bridge.listener = undefined;
  bridge.remove.mockReset();
});

it("pulls the newest revision after a notification during an in-flight request", async () => {
  const first = deferred<app.AppSnapshot>();
  const second = deferred<app.AppSnapshot>();
  bridge.getSnapshot.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
  const reportError = vi.fn();
  let dispose = () => {};
  const remote = createRoot((rootDispose) => {
    dispose = rootDispose;
    return createRemoteState(reportError);
  });

  await waitFor(() => bridge.getSnapshot.mock.calls.length === 1);
  bridge.listener?.();
  first.resolve(snapshot(1, "capture-a", "playback-a"));
  await waitFor(() => bridge.getSnapshot.mock.calls.length === 2);
  second.resolve(snapshot(2, "capture-b", "playback-b"));
  await waitFor(() => remote.state().revision === 2);

  expect(remote.state().audio.captureDeviceId).toBe("capture-b");
  expect(remote.state().audio.playbackDeviceId).toBe("playback-b");
  expect(reportError).not.toHaveBeenCalled();
  dispose();
  expect(bridge.remove).toHaveBeenCalledOnce();
});

it("reports an asynchronous snapshot error only once per id", async () => {
  const state = snapshot(1, "", "");
  state.error = new app.AppError({ id: 7, message: "network stopped" });
  bridge.getSnapshot.mockResolvedValue(state);
  const reportError = vi.fn();
  let dispose = () => {};
  const remote = createRoot((rootDispose) => {
    dispose = rootDispose;
    return createRemoteState(reportError);
  });

  await waitFor(() => remote.state().revision === 1);
  bridge.listener?.();
  await waitFor(() => bridge.getSnapshot.mock.calls.length === 2);
  expect(reportError).toHaveBeenCalledTimes(1);
  expect(reportError).toHaveBeenCalledWith("network stopped");
  dispose();
});

function snapshot(revision: number, captureDeviceId: string, playbackDeviceId: string): app.AppSnapshot {
  return new app.AppSnapshot({
    revision,
    peerId: "peer",
    nickname: "",
    audio: {
      available: true,
      running: false,
      muted: false,
      speaking: false,
      speakingPeerIds: [],
      captureDeviceId,
      playbackDeviceId,
      captureDevices: [],
      playbackDevices: [],
    },
    diagnostics: { listenAddress: "", candidates: [], stun: [] },
  });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

async function waitFor(condition: () => boolean) {
  const deadline = Date.now() + 1000;
  while (!condition()) {
    if (Date.now() >= deadline) throw new Error("condition timed out");
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
}
