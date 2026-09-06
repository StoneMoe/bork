import { Show } from "solid-js";
import { GameProxySettings } from "./GameProxySettings";
import { GameProxyStatus } from "./GameProxyStatus";
import type { SettingsPanelProps, StatusBarProps } from "./game-proxy.types";
import "./game-proxy.css";

export const settingsTabs = [{ id: "game", label: "游戏代理" }] as const;

export const initialSnapshotFields = {
  gameProxy: {
    nodeConfigured: false,
    config: {
      directories: [],
      node: {
        server: "",
        port: 4567,
        username: "",
        password: "",
        mtu: 1400,
        dns: "1.1.1.1",
        encrypt: false,
      },
    },
    status: {
      supported: false,
      state: "unsupported",
      generation: 0,
      executableCount: 0,
      directories: [],
      events: [],
      traffic: { uploadBytes: 0, downloadBytes: 0, uploadRate: 0, downloadRate: 0 },
      trafficHistory: [],
      quality: { observedAt: "", rttMs: null, lossPercent: null, history: [] },
    },
  },
};

export function SettingsPanel(props: SettingsPanelProps) {
  // Switching tabs must not discard unsaved drafts.
  return (
    <section
      id="settings-panel-game"
      class="settings-section settings-panel"
      role="tabpanel"
      aria-labelledby="settings-tab-game"
      hidden={props.activeTab !== "game"}
    >
      <GameProxySettings
        gameProxy={props.state.gameProxy}
        busy={props.busy}
        runAction={props.runAction}
      />
    </section>
  );
}

export function StatusBar(props: StatusBarProps) {
  const gameProxy = () => props.state.gameProxy;
  return (
    <Show when={!props.screenFullscreen && gameProxy().status.supported &&
      (["starting", "running", "reconnecting", "stopping", "failed"].includes(gameProxy().status.state)
        || (gameProxy().status.state === "inactive" && gameProxy().nodeConfigured))}>
      <GameProxyStatus
        status={gameProxy().status}
        nodeConfigured={gameProxy().nodeConfigured}
        hasDirectories={gameProxy().config.directories.length > 0}
        ready={props.ready}
        refresh={props.refresh}
        reportIssue={props.reportIssue}
      />
    </Show>
  );
}
