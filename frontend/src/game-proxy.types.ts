import type { IssueInput } from "./issues";
import type { ActionProps, AppState } from "./types";

export interface SettingsPanelProps extends ActionProps {
  state: AppState;
  activeTab: string;
}

export interface StatusBarProps {
  state: AppState;
  screenFullscreen: boolean;
  ready: boolean;
  refresh: () => Promise<void>;
  reportIssue: (issue: IssueInput) => void;
}
