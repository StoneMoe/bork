import * as Backend from "@wailsjs/go/app/App";
import { createMemo, createSignal, For, onCleanup, onMount, Show } from "solid-js";
import { SettingsPanel, settingsTabs as gameProxyTabs } from "@game-proxy";
import type { IssueRecord } from "./issues";
import { MicrophoneIcon, SpeakerIcon } from "./RoomControls";
import Select, { type SelectOption } from "./Select";
import { t, locale, languagePreference, setLanguagePreference, translateMessage } from "./i18n";
import type { ActionProps, AppState, Candidate, PushToTalkPreference, TrackerStatus } from "./types";

type ThemePreference = "system" | "dark" | "light";

interface SettingsProps extends ActionProps {
  state: AppState;
  close: () => void;
  pushToTalk: PushToTalkPreference;
  configurePushToTalk: (enabled: boolean, code: string) => Promise<boolean>;
  issue?: IssueRecord;
  dismissIssue: (id: string) => void;
}

const themeStorageKey = "bork.theme";

const settingsTabs = [
  { id: "audio", label: "语音" },
  { id: "device", label: "偏好" },
  ...gameProxyTabs,
  { id: "network", label: "诊断" },
] as const;

type SettingsTab = typeof settingsTabs[number]["id"];

const pushToTalkKeyLabels: Record<string, string> = {
  Backquote: "`", Minus: "-", Equal: "=", BracketLeft: "[", BracketRight: "]", Backslash: "\\", IntlBackslash: "\\",
  Semicolon: ";", Quote: "'", Comma: ",", Period: ".", Slash: "/", Space: "空格",
  Enter: "Enter", Tab: "Tab", Backspace: "Backspace", Delete: "Delete", Insert: "Insert",
  Home: "Home", End: "End", PageUp: "Page Up", PageDown: "Page Down",
  ArrowUp: "↑", ArrowDown: "↓", ArrowLeft: "←", ArrowRight: "→",
  NumpadDivide: "Num /", NumpadMultiply: "Num *", NumpadSubtract: "Num -", NumpadAdd: "Num +",
  NumpadEnter: "Num Enter", NumpadDecimal: "Num .",
};

function formatPushToTalkKey(code: string) {
  if (pushToTalkKeyLabels[code]) return t(pushToTalkKeyLabels[code]);
  if (code.startsWith("Key") && code.length === 4) return code.slice(3);
  if (code.startsWith("Digit") && code.length === 6) return code.slice(5);
  if (code.startsWith("Numpad")) return `Num ${code.slice(6)}`;
  return code;
}

function audioDeviceOptions(devices: readonly { id: string; name: string; isDefault: boolean }[]): SelectOption[] {
  return [
    { value: "", label: t("系统默认") },
    ...devices.map((device) => ({ value: device.id, label: device.isDefault ? t("{name}（默认）", { name: device.name }) : device.name })),
  ];
}

export default function Settings(props: SettingsProps) {
  const initialTheme = document.documentElement.dataset.theme;
  const [activeTab, setActiveTab] = createSignal<SettingsTab>("audio");
  const [theme, setTheme] = createSignal<ThemePreference>(
    initialTheme === "dark" || initialTheme === "light" ? initialTheme : "system",
  );
  const tabButtons: Partial<Record<SettingsTab, HTMLButtonElement>> = {};
  let settingsDrawer: HTMLElement | undefined;
  const audio = () => props.state.audio;
  const diagnostics = () => props.state.diagnostics;
  const candidates = () => diagnostics().candidates || [];
  const stun = () => diagnostics().stun || [];
  const trackers = () => diagnostics().tracker || [];
  const trackerGroups = createMemo(() => {
    const groups: Array<{ provider: string; items: TrackerStatus[] }> = [];
    const byProvider = new Map<string, TrackerStatus[]>();
    for (const status of trackers()) {
      let items = byProvider.get(status.provider);
      if (!items) {
        items = [];
        byProvider.set(status.provider, items);
        groups.push({ provider: status.provider, items });
      }
      items.push(status);
    }
    return groups;
  });
  const connectivity = () => diagnostics().connectivity;
  const discoveryHints = () => connectivity()?.discoveryHints || [];
  const [nickname, setNickname] = createSignal(props.state.nickname);
  const [capturingPushToTalkKey, setCapturingPushToTalkKey] = createSignal(false);
  const [now, setNow] = createSignal(Date.now());
  const [dismissedTrackerTooltip, setDismissedTrackerTooltip] = createSignal("");
  const clock = window.setInterval(() => setNow(Date.now()), 1000);
  onCleanup(() => window.clearInterval(clock));
  onMount(() => {
    tabButtons.audio?.focus({ preventScroll: true });

    let syncingAudioDevices = false;
    const syncAudioDevices = async () => {
      if (!props.ready || props.busy || syncingAudioDevices) return;
      syncingAudioDevices = true;
      try {
        await Backend.SyncAudioDevices();
      } catch {
        // This background refresh is best effort; user actions still report their own errors.
      } finally {
        syncingAudioDevices = false;
      }
    };
    void syncAudioDevices();
    const deviceSync = window.setInterval(() => void syncAudioDevices(), 2000);
    onCleanup(() => window.clearInterval(deviceSync));
  });

  async function saveNickname() {
    if (nickname() === props.state.nickname) return;
    if (!await props.runAction(() => Backend.SetNickname(nickname()))) setNickname(props.state.nickname);
  }

  function updateTheme(next: ThemePreference) {
    setTheme(next);
    if (next === "system") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = next;
    try {
      if (next === "system") localStorage.removeItem(themeStorageKey);
      else localStorage.setItem(themeStorageKey, next);
    } catch { /* storage unavailable */ }
  }

  function dismissLatestIssue() {
    const issue = props.issue;
    if (!issue?.id) return;
    props.dismissIssue(issue.id);
    queueMicrotask(() => {
      const active = document.activeElement;
      if (active instanceof HTMLElement && active !== document.body && active.isConnected) return;
      tabButtons[activeTab()]?.focus({ preventScroll: true });
    });
  }

  function moveTab(event: KeyboardEvent, index: number) {
    let next = index;
    if (event.key === "ArrowRight") next = (index + 1) % settingsTabs.length;
    else if (event.key === "ArrowLeft") next = (index - 1 + settingsTabs.length) % settingsTabs.length;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = settingsTabs.length - 1;
    else return;
    event.preventDefault();
    const tab = settingsTabs[next].id;
    setActiveTab(tab);
    queueMicrotask(() => tabButtons[tab]?.focus({ preventScroll: true }));
  }

  function capturePushToTalkKey(event: KeyboardEvent) {
    if (!capturingPushToTalkKey()) return;
    event.preventDefault();
    event.stopPropagation();
    if (event.code === "Escape") {
      setCapturingPushToTalkKey(false);
      return;
    }
    if (event.repeat || event.code === "Unidentified"
      || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    setCapturingPushToTalkKey(false);
    void props.configurePushToTalk(props.pushToTalk.enabled, event.code);
  }

  function handleSettingsKeyDown(event: KeyboardEvent) {
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      props.close();
      return;
    }
    if (event.key !== "Tab" || !settingsDrawer) return;
    const focusable = Array.from(settingsDrawer.querySelectorAll<HTMLElement>(
      "button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])",
    )).filter((element) => element.tabIndex >= 0 && element.getClientRects().length > 0);
    if (focusable.length === 0) return;
    const active = document.activeElement;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && (active === first || !settingsDrawer.contains(active))) {
      event.preventDefault();
      last.focus({ preventScroll: true });
    } else if (!event.shiftKey && (active === last || !settingsDrawer.contains(active))) {
      event.preventDefault();
      first.focus({ preventScroll: true });
    }
  }

  return (
    <div
      class="settings-layer"
      onKeyDown={handleSettingsKeyDown}
    >
      <button class="settings-backdrop" type="button" tabindex="-1" aria-label={t("关闭设置")} onClick={props.close} />
      <aside ref={settingsDrawer} class="settings-drawer" role="dialog" aria-modal="true" aria-label={t("设置")}>
        <nav class="settings-tabs" role="tablist" aria-label={t("设置分类")}>
          <For each={settingsTabs}>{(tab, index) => (
            <button
              ref={(element) => { tabButtons[tab.id] = element; }}
              id={`settings-tab-${tab.id}`}
              class="settings-tab"
              classList={{ active: activeTab() === tab.id }}
              type="button"
              role="tab"
              aria-selected={activeTab() === tab.id}
              aria-controls={`settings-panel-${tab.id}`}
              tabindex={activeTab() === tab.id ? 0 : -1}
              onClick={() => setActiveTab(tab.id)}
              onKeyDown={(event) => moveTab(event, index())}
            >{t(tab.label)}</button>
          )}</For>
        </nav>
        <Show when={props.issue}>{(issue) => (
          <div class="settings-issue attention-item" classList={{ [issue().level]: true }}>
            <div>
              <strong>{issue().title}</strong>
              <p>{issue().message}</p>
            </div>
            <button type="button" aria-label={t("关闭：{title}", { title: issue().title })} onClick={dismissLatestIssue}>
              <SettingsCloseIcon />
            </button>
          </div>
        )}</Show>
        <div class="settings-content" tabindex="0">
          <section
          id="settings-panel-audio"
          class="settings-section settings-panel"
          role="tabpanel"
          aria-labelledby="settings-tab-audio"
          hidden={activeTab() !== "audio"}
        >
          <div class="audio-device-field audio-device-row">
            <span id="playback-device-label" class="audio-device-icon">
              <SpeakerIcon muted={false} />
              <span class="visually-hidden">{t("扬声器")}</span>
            </span>
            <Select
              id="playback-device-select"
              value={audio().playbackDeviceId}
              options={audioDeviceOptions(audio().playbackDevices)}
              labelledBy="playback-device-label"
              disabled={props.busy || !props.ready}
              onChange={(value) => void props.runAction(
                () => Backend.SetAudioDevices(audio().captureDeviceId, value),
                { type: "audio" },
              )}
            />
          </div>
          <div class="audio-device-field audio-device-row">
            <span id="capture-device-label" class="audio-device-icon">
              <MicrophoneIcon muted={false} />
              <span class="visually-hidden">{t("麦克风")}</span>
            </span>
            <Select
              id="capture-device-select"
              value={audio().captureDeviceId}
              options={audioDeviceOptions(audio().captureDevices)}
              labelledBy="capture-device-label"
              disabled={props.busy || !props.ready}
              onChange={(value) => void props.runAction(
                () => Backend.SetAudioDevices(value, audio().playbackDeviceId),
                { type: "audio" },
              )}
            />
          </div>
          <Show when={
            props.state.audio.captureDevices.length === 0
            || props.state.audio.playbackDevices.length === 0
          }>
            <p class="empty-diagnostic audio-device-empty">{t("没有可用的麦克风或扬声器。")}</p>
          </Show>
          <div class="audio-toggles">
            <label class="setting-row audio-toggle">
              <span>
                <strong>{t("按键说话")}</strong>
                <small>{t("按下指定的快捷键才启用麦克风")}</small>
              </span>
              <input
                type="checkbox"
                checked={props.pushToTalk.enabled}
                disabled={props.busy || !props.ready}
                onChange={async (event) => {
                  const input = event.currentTarget;
                  const checked = input.checked;
                  if (!await props.configurePushToTalk(checked, props.pushToTalk.code)) input.checked = !checked;
                }}
              />
            </label>
            <Show when={props.pushToTalk.enabled}>
              <div class="setting-row audio-toggle">
                <span>
                  <strong>{t("说话键")}</strong>
                  <small>{t("为按键说话设置一个快捷键")}</small>
                </span>
                <button
                  class="push-to-talk-key-button"
                  type="button"
                  disabled={props.busy || !props.ready}
                  aria-label={capturingPushToTalkKey() ? t("请按一个按键，按 Escape 取消") : t("说话按键 {key}，点击更改", { key: formatPushToTalkKey(props.pushToTalk.code) })}
                  title={capturingPushToTalkKey() ? t("按 Escape 取消") : t("更改说话按键")}
                  onClick={() => setCapturingPushToTalkKey(true)}
                  onKeyDown={capturePushToTalkKey}
                  onBlur={() => setCapturingPushToTalkKey(false)}
                >
                  <Show when={!capturingPushToTalkKey()} fallback={<span>{t("请按键…")}</span>}>
                    <kbd>{formatPushToTalkKey(props.pushToTalk.code)}</kbd>
                  </Show>
                </button>
                <span class="visually-hidden" role="status" aria-live="polite">
                  {capturingPushToTalkKey() ? t("请按一个非修饰键，按 Escape 取消") : ""}
                </span>
              </div>
            </Show>
            <label class="setting-row audio-toggle">
              <span>
                <strong>{t("回声消除")}</strong>
                <small>{t("减少扬声器声音被麦克风再次收录的可能性")}</small>
              </span>
              <input
                type="checkbox"
                checked={audio().echoCancellation}
                disabled={props.busy || !props.ready}
                onChange={async (event) => {
                  const input = event.currentTarget;
                  const checked = input.checked;
                  if (!await props.runAction(() => Backend.SetEchoCancellation(checked), { type: "audio" })) input.checked = !checked;
                }}
              />
            </label>
            <label class="setting-row audio-toggle">
              <span>
                <strong>{t("智能降噪")}</strong>
                <small>{t("抑制键盘、风扇等常见背景噪声")}</small>
              </span>
              <input
                type="checkbox"
                checked={audio().noiseSuppression}
                disabled={props.busy || !props.ready}
                onChange={async (event) => {
                  const input = event.currentTarget;
                  const checked = input.checked;
                  if (!await props.runAction(() => Backend.SetNoiseSuppression(checked), { type: "audio" })) input.checked = !checked;
                }}
              />
            </label>
            <label class="setting-row audio-toggle">
              <span>
                <strong>{t("响度平衡")}</strong>
                <small>{t("自动平衡不同成员的音量大小")}</small>
              </span>
              <input
                type="checkbox"
                checked={audio().remoteLoudnessNormalization}
                disabled={props.busy || !props.ready}
                onChange={async (event) => {
                  const input = event.currentTarget;
                  const checked = input.checked;
                  if (!await props.runAction(() => Backend.SetRemoteLoudnessNormalization(checked), { type: "audio" })) input.checked = !checked;
                }}
              />
            </label>
          </div>
          </section>
          <section
          id="settings-panel-device"
          class="settings-section settings-panel"
          role="tabpanel"
          aria-labelledby="settings-tab-device"
          hidden={activeTab() !== "device"}
        >
          <div class="nickname-form">
            <label for="nickname">{t("昵称")}</label>
            <input
              id="nickname"
              autocomplete="off"
              autocapitalize="off"
              maxlength={32}
              spellcheck={false}
                placeholder={t("未设置")}
                value={nickname()}
                disabled={props.busy || !props.ready}
                onInput={(event) => setNickname(event.currentTarget.value)}
              onChange={() => void saveNickname()}
              onKeyDown={(event) => { if (event.key === "Enter") event.currentTarget.blur(); }}
            />
          </div>
          <div class="audio-device-field">
            <span id="theme-label">{t("界面主题")}</span>
            <div class="theme-button-group" role="group" aria-labelledby="theme-label">
              <button type="button" aria-pressed={theme() === "system"} onClick={() => updateTheme("system")}>{t("跟随系统")}</button>
              <button type="button" aria-pressed={theme() === "dark"} onClick={() => updateTheme("dark")}>{t("深黑")}</button>
              <button type="button" aria-pressed={theme() === "light"} onClick={() => updateTheme("light")}>{t("浅灰")}</button>
            </div>
          </div>
          <div class="audio-device-field">
            <span id="language-label">{t("界面语言")}</span>
            <Select
              id="language-select"
              value={languagePreference()}
              options={[
                { value: "auto", label: t("自动") },
                { value: "zh-CN", label: "简体中文" },
                { value: "en", label: "English" },
              ]}
              labelledBy="language-label"
              onChange={(value) => setLanguagePreference(value as ReturnType<typeof languagePreference>)}
            />
          </div>
          </section>
          <SettingsPanel
            state={props.state}
            activeTab={activeTab()}
            busy={props.busy}
            ready={props.ready}
            runAction={props.runAction}
          />
          <section
          id="settings-panel-network"
          class="settings-section settings-panel"
          role="tabpanel"
          aria-labelledby="settings-tab-network"
          hidden={activeTab() !== "network"}
        >
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>{t("版本")}</span></div>
            <code class="diagnostic-value">{props.state.version}</code>
          </div>
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>{t("本机端点")}</span></div>
            <Show when={diagnostics().listenAddress} fallback={
              <small class="empty-diagnostic">{props.state.room ? t("正在打开本机 UDP 端点。") : t("加入房间后打开 UDP 端点。")}</small>
            }>
              <code class="diagnostic-value">{t("{address}（UDP）", { address: diagnostics().listenAddress })}</code>
            </Show>
          </div>
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>{t("本机候选地址")}</span><b>{candidates().length.toLocaleString(locale())}</b></div>
            <Show when={candidates().length > 0} fallback={
              <small class="empty-diagnostic">{props.state.room ? t("尚未发现可用的本机候选地址。") : t("加入房间后开始收集本机候选地址。")}</small>
            }>
              <ol class="candidate-list">
                <For each={candidates()}>{(candidate) => <CandidateRow candidate={candidate} />}</For>
              </ol>
            </Show>
          </div>
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>{t("远端候选地址")}</span><b>{discoveryHints().length.toLocaleString(locale())}</b></div>
            <Show when={discoveryHints().length > 0} fallback={
              <small class="empty-diagnostic">{props.state.room ? t("尚未收到其他成员的候选地址。") : t("加入房间后开始收集远端候选地址。")}</small>
            }>
              <ol class="candidate-list">
                <For each={discoveryHints()}>{(hint) => (
                  <li>
                    <code>{hint.address}</code>
                    <div class="address-origin"><b>{discoveryHintSourceLabel(hint.source)}</b><small>{formatDiscoveryHintExpiry(hint.expiresAt, now())}</small></div>
                  </li>
                )}</For>
              </ol>
            </Show>
          </div>
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>{t("STUN 探测")}</span></div>
            <Show when={stun().length > 0} fallback={
              <small class="empty-diagnostic">{props.state.room ? t("尚未获得 STUN 探测结果。") : t("加入房间后开始 STUN 探测。")}</small>
            }>
              <ol class="stun-list">
                <For each={stun()}>{(result) => (
                  <li classList={{ failed: !result.mappedAddress }} title={translateMessage(result.error || "")}>
                    <span>{result.server}{result.family && ` · ${result.family === "ipv6" ? "IPv6" : "IPv4"}`}</span>
                    <b>{result.mappedAddress ? `${(result.rttMillis || 1).toLocaleString(locale())} ms` : t("失败")}</b>
                  </li>
                )}</For>
              </ol>
            </Show>
          </div>
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>{t("Tracker 公告")}</span></div>
            <Show when={trackers().length > 0} fallback={
              <small class="empty-diagnostic">{props.state.room ? t("尚未产生 Tracker announce 记录。") : t("加入房间后开始 Tracker announce。")}</small>
            }>
              <div class="tracker-groups">
                <For each={trackerGroups().map((group) => group.provider)}>{(provider, groupIndex) => {
                const group = () => trackerGroups().find((candidate) => candidate.provider === provider)!;
                return (
                  <article class="tracker-group">
                    <header><strong title={group().provider}>{group().provider}</strong><small>{t("{count} 个候选地址", { count: group().items.length.toLocaleString(locale()) })}</small></header>
                    <For each={group().items.map((tracker) => tracker.candidate)}>{(candidate, trackerIndex) => {
                      const tracker = () => group().items.find((status) => status.candidate === candidate)!;
                      const tooltipID = () => `tracker-peers-${groupIndex()}-${trackerIndex()}`;
                      const tooltipKey = `${provider}\n${candidate}`;
                      return (
                        <div
                          class="tracker-candidate-row"
                          classList={{ failed: Boolean(tracker().error), "tooltip-dismissed": dismissedTrackerTooltip() === tooltipKey }}
                          tabindex="0"
                          aria-describedby={tooltipID()}
                          onFocus={(event) => {
                            if (dismissedTrackerTooltip() === tooltipKey) setDismissedTrackerTooltip("");
                            positionTrackerPeerPopover(event.currentTarget);
                          }}
                          onBlur={(event) => {
                            if (dismissedTrackerTooltip() === tooltipKey && !event.currentTarget.matches(":hover")) setDismissedTrackerTooltip("");
                          }}
                          onPointerEnter={(event) => positionTrackerPeerPopover(event.currentTarget)}
                          onPointerLeave={(event) => {
                            if (dismissedTrackerTooltip() === tooltipKey && !event.currentTarget.matches(":focus-visible")) setDismissedTrackerTooltip("");
                          }}
                          onKeyDown={(event) => {
                            if (event.key !== "Escape") return;
                            if (dismissedTrackerTooltip() === tooltipKey) return;
                            event.preventDefault();
                            event.stopPropagation();
                            setDismissedTrackerTooltip(tooltipKey);
                          }}
                        >
                          <span><small>{t("请求地址")}</small><code>{tracker().candidate || t("等待候选地址")}</code></span>
                          <span class="tracker-next"><small>{t("下次公告")}</small><b>{tracker().nextAnnounce ? formatRelativeTime(tracker().nextAnnounce!, now()) : t("等待")}</b></span>
                          <div id={tooltipID()} class="tracker-peer-popover" role="tooltip">
                            <strong>{tracker().error ? t("Tracker 错误") : t("本次返回的地址")}</strong>
                            <Show when={!tracker().error} fallback={<p>{translateMessage(tracker().error || "")}</p>}>
                              <Show when={(tracker().peerAddresses ?? []).length > 0} fallback={<p>{t("本次 announce 没有返回 Peer 地址。")}</p>}>
                                <ul><For each={tracker().peerAddresses ?? []}>{(address) => <li><code>{address}</code></li>}</For></ul>
                              </Show>
                            </Show>
                          </div>
                        </div>
                      );
                    }}</For>
                  </article>
                );
                }}</For>
              </div>
            </Show>
          </div>
          </section>
        </div>
      </aside>
    </div>
  );
}

function SettingsCloseIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>;
}

function formatRelativeTime(value: string, now: number): string {
  const target = Date.parse(value);
  if (!Number.isFinite(target)) return t("等待 announce");
  const seconds = Math.max(0, Math.ceil((target - now) / 1000));
  return t("{seconds} 秒后", { seconds: seconds.toLocaleString(locale()) });
}

function positionTrackerPeerPopover(row: HTMLElement) {
  const popover = row.querySelector<HTMLElement>(".tracker-peer-popover");
  const viewport = row.closest<HTMLElement>(".settings-content");
  if (!popover || !viewport) return;
  const rowBounds = row.getBoundingClientRect();
  const viewportBounds = viewport.getBoundingClientRect();
  const spaceAbove = rowBounds.top - viewportBounds.top;
  const spaceBelow = viewportBounds.bottom - rowBounds.bottom;
  popover.classList.toggle("above", popover.offsetHeight > spaceBelow && spaceAbove > spaceBelow);
}

function CandidateRow(props: { candidate: Candidate }) {
  const typeLabel = () => {
    if (props.candidate.type === "port-mapped") return t("端口映射");
    if (props.candidate.type === "stun") return t("STUN 响应");
    if (props.candidate.type === "nic") return t("网卡");
    return t("未知");
  };
  return (
    <li>
      <code>{props.candidate.address}</code>
      <div class="address-origin"><b>{typeLabel()}</b><small>{props.candidate.interface || props.candidate.source || props.candidate.family || t("系统")}</small></div>
    </li>
  );
}

function discoveryHintSourceLabel(source: string): string {
  return t(({ "historical-remote": "历史远端", local: "本机", mdns: "mDNS", tracker: "Tracker", topology: "拓扑" } as Record<string, string>)[source] || "发现");
}

function formatDiscoveryHintExpiry(value: string | undefined, now: number): string {
  if (!value) return t("会话期间有效");
  const target = Date.parse(value);
  if (!Number.isFinite(target)) return t("有效期未知");
  const seconds = Math.max(0, Math.ceil((target-now) / 1000));
  return seconds === 0 ? t("即将过期") : t("线索剩余 {seconds} 秒", { seconds: seconds.toLocaleString(locale()) });
}
