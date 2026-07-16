import type { app, audio, endpoint } from "@wailsjs/go/models";

export type AppState = app.AppSnapshot;
export type AudioStatus = audio.Status;
export type Candidate = endpoint.Candidate;
export type Diagnostics = app.Diagnostics;
export type RemotePeer = app.RemotePeer;

export interface FriendlyStatus {
  badge: string;
  title: string;
  detail: string;
}
