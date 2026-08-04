import type { app, endpoint } from "@wailsjs/go/models";

export type AppState = app.AppSnapshot;
export type Candidate = endpoint.Candidate;
export type RemotePeer = app.RemotePeer;

export interface FriendlyStatus {
  badge: string;
  title?: string;
  detail?: string;
}
