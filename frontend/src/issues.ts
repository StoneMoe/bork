import type { app } from "@wailsjs/go/models";

export type IssueType = "general" | "room" | "audio" | "network" | "screen";
export type IssueLevel = "warning" | "error";

export interface IssueInput {
  type: IssueType;
  level?: IssueLevel;
  title?: string;
  message: string;
}

export interface IssueContext {
  type?: IssueType;
  title?: string;
  onError?: (message: string) => void;
}

export interface IssueRecord {
  id?: string;
  type: IssueType;
  level: IssueLevel;
  title: string;
  message: string;
}

const defaultTitles: Record<IssueType, string> = {
  general: "出现问题",
  room: "房间错误",
  audio: "音频错误",
  network: "网络错误",
  screen: "屏幕分享错误",
};

function issueRecord(input: IssueInput, id?: string): IssueRecord {
  return {
    id,
    type: input.type,
    level: input.level ?? "error",
    title: input.title ?? defaultTitles[input.type],
    message: input.message,
  };
}

export function appendIssue(existing: readonly IssueRecord[], input: IssueInput, id: string): IssueRecord[] {
  return [issueRecord(input, id), ...existing].slice(0, 32);
}

export function collectStateIssues(state: app.AppSnapshot): IssueRecord[] {
  // These records follow the snapshot directly. They disappear when their
  // source recovers, so they intentionally do not support dismissal.
  return [
    ...(state.audio.error ? [issueRecord({ type: "audio", message: state.audio.error })] : []),
    // Diagnostics are an archived postmortem snapshot after a terminal room
    // failure. Only treat them as current while the room is still active.
    ...(state.room ? collectNetworkIssues(state.diagnostics) : []),
  ];
}

function collectNetworkIssues(diagnostics: app.Diagnostics): IssueRecord[] {
  const issues: IssueRecord[] = [];
  if (diagnostics.networkError) {
    issues.push(issueRecord({ type: "network", message: diagnostics.networkError }));
  }
  if (diagnostics.discoveryError) {
    issues.push(issueRecord({
      type: "network",
      level: "warning",
      title: "成员发现错误",
      message: diagnostics.discoveryError,
    }));
  }
  if (diagnostics.portMappingError) {
    issues.push(issueRecord({
      type: "network",
      level: "warning",
      title: "端口映射错误",
      message: diagnostics.portMappingError,
    }));
  }

  const stunResults = diagnostics.stun ?? [];
  if (allFailed(stunResults, (result) => !result.mappedAddress)) {
    issues.push(issueRecord({
      type: "network",
      level: "warning",
      title: "STUN 探测全部失败",
      message: `${stunResults.length} 条 STUN 探测均失败，请前往诊断查看详情。`,
    }));
  }

  const trackerStatuses = diagnostics.tracker ?? [];
  if (allFailed(trackerStatuses, (tracker) => Boolean(tracker.error))) {
    issues.push(issueRecord({
      type: "network",
      level: "warning",
      title: "Tracker 公告全部失败",
      message: `${trackerStatuses.length} 条 Tracker 公告均失败，请前往诊断查看详情。`,
    }));
  }
  return issues;
}

function allFailed<T>(values: readonly T[], failed: (value: T) => boolean) {
  return values.length > 0 && values.every(failed);
}
