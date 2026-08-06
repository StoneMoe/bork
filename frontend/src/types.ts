import type { app, endpoint, tracker } from "@wailsjs/go/models";

export type AppState = app.AppSnapshot;
export type Candidate = endpoint.Candidate;
export type RemotePeer = app.RemotePeer;
export type FileTransfer = app.FileTransfer;
export type TrackerStatus = tracker.ProviderStatus;

export interface ActionProps {
  busy: boolean;
  ready: boolean;
  runAction: (action: () => Promise<void>) => Promise<boolean>;
}

export interface FriendlyStatus {
  title?: string;
  detail?: string;
}
