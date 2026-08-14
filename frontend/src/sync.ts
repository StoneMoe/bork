import { createSignal, onCleanup, onMount } from "solid-js";
import { GetSnapshot } from "@wailsjs/go/app/App";
import { app } from "@wailsjs/go/models";
import { EventsOn } from "@wailsjs/runtime/runtime";
import type { AppState } from "./types";

const emptyState = new app.AppSnapshot({
  nickname: "",
  audio: {
    available: false,
    running: false,
    captureMuted: false,
    playbackMuted: false,
    captureGain: 100,
    captureLevel: 0,
    captureClipped: false,
    playbackGain: 100,
    echoCancellation: true,
    noiseSuppression: true,
    remoteLoudnessNormalization: true,
    speaking: false,
    speakingPeerIds: [],
    captureDeviceId: "",
    playbackDeviceId: "",
    captureDevices: [],
    playbackDevices: [],
  },
  diagnostics: {
    listenAddress: "",
    candidates: [],
    stun: [],
    tracker: [],
    connectivity: {
      discoveryHints: [],
    },
  },
});

export function createRemoteState(reportError: (message: string) => void) {
  const [state, setState] = createSignal<AppState>(emptyState);
  const [ready, setReady] = createSignal(false);
  let disposed = false;
  let requested = false;
  let pulling: Promise<void> | undefined;
  let lastErrorId = 0;

  function refresh(): Promise<void> {
    requested = true;
    if (pulling) return pulling;
    pulling = (async () => {
      while (requested && !disposed) {
        requested = false;
        try {
          const next = await GetSnapshot();
          if (disposed) return;
          setState(next);
          setReady(true);
          if (next.error && next.error.id > lastErrorId) {
            lastErrorId = next.error.id;
            reportError(next.error.message);
          }
        } catch (cause) {
          if (!disposed) reportError(cause instanceof Error ? cause.message : String(cause));
        }
      }
    })().finally(() => {
      pulling = undefined;
      if (requested && !disposed) return refresh();
    });
    return pulling;
  }

  onMount(() => {
    const removeListener = EventsOn("bork:state-changed", () => {
      void refresh();
    });
    void refresh();
    onCleanup(() => {
      disposed = true;
      removeListener();
    });
  });

  return { state, ready, refresh };
}
