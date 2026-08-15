import { For, Show, createMemo, createSignal, onCleanup, onMount } from "solid-js";
import * as Backend from "@wailsjs/go/app/App";
import type { ActionProps, AppState, Candidate, TrackerStatus } from "./types";

interface SettingsProps extends ActionProps {
  state: AppState;
  close: () => void;
}

const settingsTabs = [
  { id: "device", label: "我" },
  { id: "audio", label: "语音" },
  { id: "network", label: "诊断" },
] as const;

type SettingsTab = typeof settingsTabs[number]["id"];

export default function Settings(props: SettingsProps) {
  const [activeTab, setActiveTab] = createSignal<SettingsTab>("device");
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
  const diagnosticErrors = () => [diagnostics().networkError, diagnostics().discoveryError, diagnostics().portMappingError]
    .filter((message): message is string => Boolean(message));
  const [nickname, setNickname] = createSignal(props.state.nickname);
  const [now, setNow] = createSignal(Date.now());
  const [dismissedTrackerTooltip, setDismissedTrackerTooltip] = createSignal("");
  const clock = window.setInterval(() => setNow(Date.now()), 1000);
  onCleanup(() => window.clearInterval(clock));
  onMount(() => tabButtons.device?.focus({ preventScroll: true }));

  async function saveNickname() {
    if (nickname() === props.state.nickname) return;
    if (!await props.runAction(() => Backend.SetNickname(nickname()))) setNickname(props.state.nickname);
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
      <button class="settings-backdrop" type="button" tabindex="-1" aria-label="关闭设置" onClick={props.close} />
      <aside ref={settingsDrawer} class="settings-drawer" role="dialog" aria-modal="true" aria-label="设置">
        <nav class="settings-tabs" role="tablist" aria-label="设置分类">
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
            >{tab.label}</button>
          )}</For>
        </nav>
        <div class="settings-content" tabindex="0">
          <section
          id="settings-panel-audio"
          class="settings-section settings-panel"
          role="tabpanel"
          aria-labelledby="settings-tab-audio"
          hidden={activeTab() !== "audio"}
        >
          <div class="settings-section-heading">
            <button
              class="section-action"
              type="button"
              disabled={props.busy || !props.ready || props.state.audio.running}
              onClick={() => props.runAction(Backend.RefreshAudioDevices)}
            >刷新</button>
          </div>
          <label class="audio-device-field">
            <span>麦克风</span>
            <select
              value={audio().captureDeviceId}
              disabled={props.busy || !props.ready || props.state.audio.running}
              onChange={(event) => props.runAction(() => Backend.SetAudioDevices(event.currentTarget.value, audio().playbackDeviceId))}
            >
              <option value="">系统默认</option>
              <For each={audio().captureDevices}>{(device) => (
                <option value={device.id}>{device.name}{device.isDefault ? "（默认）" : ""}</option>
              )}</For>
            </select>
          </label>
          <label class="audio-device-field">
            <span>扬声器</span>
            <select
              value={audio().playbackDeviceId}
              disabled={props.busy || !props.ready || props.state.audio.running}
              onChange={(event) => props.runAction(() => Backend.SetAudioDevices(audio().captureDeviceId, event.currentTarget.value))}
            >
              <option value="">系统默认</option>
              <For each={audio().playbackDevices}>{(device) => (
                <option value={device.id}>{device.name}{device.isDefault ? "（默认）" : ""}</option>
              )}</For>
            </select>
          </label>
          <div class="audio-toggles">
            <label class="setting-row audio-toggle">
              <span>
                <strong>回声消除</strong>
                <small>减少扬声器声音被麦克风再次收录。</small>
              </span>
              <input
                type="checkbox"
                checked={audio().echoCancellation}
                disabled={props.busy || !props.ready}
                onChange={async (event) => {
                  const input = event.currentTarget;
                  const checked = input.checked;
                  if (!await props.runAction(() => Backend.SetEchoCancellation(checked))) input.checked = !checked;
                }}
              />
            </label>
            <label class="setting-row audio-toggle">
              <span>
                <strong>智能降噪</strong>
                <small>降低键盘、风扇等持续背景噪声。</small>
              </span>
              <input
                type="checkbox"
                checked={audio().noiseSuppression}
                disabled={props.busy || !props.ready}
                onChange={async (event) => {
                  const input = event.currentTarget;
                  const checked = input.checked;
                  if (!await props.runAction(() => Backend.SetNoiseSuppression(checked))) input.checked = !checked;
                }}
              />
            </label>
            <label class="setting-row audio-toggle">
              <span>
                <strong>响度平衡</strong>
                <small>自动平衡不同成员的播放响度。</small>
              </span>
              <input
                type="checkbox"
                checked={audio().remoteLoudnessNormalization}
                disabled={props.busy || !props.ready}
                onChange={async (event) => {
                  const input = event.currentTarget;
                  const checked = input.checked;
                  if (!await props.runAction(() => Backend.SetRemoteLoudnessNormalization(checked))) input.checked = !checked;
                }}
              />
            </label>
          </div>
          <Show when={props.state.audio.error}>
            <p class="diagnostic-error">{props.state.audio.error}</p>
          </Show>
          <Show when={!props.state.audio.available && !props.state.audio.error}>
            <p class="empty-diagnostic">没有可用的麦克风或扬声器。</p>
          </Show>
          </section>
          <section
          id="settings-panel-device"
          class="settings-section settings-panel"
          role="tabpanel"
          aria-labelledby="settings-tab-device"
          hidden={activeTab() !== "device"}
        >
          <div class="nickname-form">
            <label for="nickname">房间昵称</label>
            <input
              id="nickname"
              autocomplete="off"
              autocapitalize="off"
              maxlength={32}
              spellcheck={false}
                placeholder="未设置"
                value={nickname()}
                disabled={props.busy || !props.ready}
                onInput={(event) => setNickname(event.currentTarget.value)}
              onChange={() => void saveNickname()}
              onKeyDown={(event) => { if (event.key === "Enter") event.currentTarget.blur(); }}
            />
            <small>修改后自动保存并对其他成员可见。</small>
          </div>
          </section>
          <section
          id="settings-panel-network"
          class="settings-section settings-panel"
          role="tabpanel"
          aria-labelledby="settings-tab-network"
          hidden={activeTab() !== "network"}
        >
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>本机端点</span></div>
            <Show when={diagnostics().listenAddress} fallback={
              <small class="empty-diagnostic">{props.state.room ? "正在打开本机 UDP 端点。" : "加入房间后打开 UDP 端点。"}</small>
            }>
              <code class="diagnostic-value">{`${diagnostics().listenAddress}（UDP）`}</code>
            </Show>
          </div>
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>本机候选地址</span><b>{candidates().length}</b></div>
            <Show when={candidates().length > 0} fallback={
              <small class="empty-diagnostic">{props.state.room ? "尚未发现可用的本机候选地址。" : "加入房间后开始收集本机候选地址。"}</small>
            }>
              <ol class="candidate-list">
                <For each={candidates()}>{(candidate) => <CandidateRow candidate={candidate} />}</For>
              </ol>
            </Show>
          </div>
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>远端候选地址</span><b>{discoveryHints().length}</b></div>
            <Show when={discoveryHints().length > 0} fallback={
              <small class="empty-diagnostic">{props.state.room ? "尚未收到其他成员的候选地址。" : "加入房间后开始收集远端候选地址。"}</small>
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
            <div class="diagnostic-heading"><span>STUN 探测</span></div>
            <Show when={stun().length > 0} fallback={
              <small class="empty-diagnostic">{props.state.room ? "尚未获得 STUN 探测结果。" : "加入房间后开始 STUN 探测。"}</small>
            }>
              <ol class="stun-list">
                <For each={stun()}>{(result) => (
                  <li classList={{ failed: !result.mappedAddress }} title={result.error || ""}>
                    <span>{result.server}{result.family && ` · ${result.family === "ipv6" ? "IPv6" : "IPv4"}`}</span>
                    <b>{result.mappedAddress ? `${result.rttMillis || 1} ms` : "失败"}</b>
                  </li>
                )}</For>
              </ol>
            </Show>
          </div>
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>Tracker 公告</span></div>
            <Show when={trackers().length > 0} fallback={
              <small class="empty-diagnostic">{props.state.room ? "尚未产生 Tracker announce 记录。" : "加入房间后开始 Tracker announce。"}</small>
            }>
              <div class="tracker-groups">
                <For each={trackerGroups().map((group) => group.provider)}>{(provider, groupIndex) => {
                const group = () => trackerGroups().find((candidate) => candidate.provider === provider)!;
                return (
                  <article class="tracker-group">
                    <header><strong title={group().provider}>{group().provider}</strong><small>{group().items.length} 个候选地址</small></header>
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
                          <span><small>请求地址</small><code>{tracker().candidate || "等待候选地址"}</code></span>
                          <span class="tracker-next"><small>下次公告</small><b>{tracker().nextAnnounce ? formatRelativeTime(tracker().nextAnnounce!, now()) : "等待"}</b></span>
                          <div id={tooltipID()} class="tracker-peer-popover" role="tooltip">
                            <strong>{tracker().error ? "Tracker 错误" : "本次返回的地址"}</strong>
                            <Show when={!tracker().error} fallback={<p>{tracker().error}</p>}>
                              <Show when={(tracker().peerAddresses ?? []).length > 0} fallback={<p>本次 announce 没有返回 Peer 地址。</p>}>
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
          <Show when={diagnosticErrors().length > 0}>
            <div class="diagnostic-section">
              <div class="diagnostic-heading"><span>错误</span><b>{diagnosticErrors().length}</b></div>
              <For each={diagnosticErrors()}>{(message) => <p class="diagnostic-error">{message}</p>}</For>
            </div>
          </Show>
          </section>
        </div>
      </aside>
    </div>
  );
}

function formatRelativeTime(value: string, now: number): string {
  const target = Date.parse(value);
  if (!Number.isFinite(target)) return "等待 announce";
  const seconds = Math.max(0, Math.ceil((target - now) / 1000));
  return `${seconds} 秒后`;
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
    if (props.candidate.type === "port-mapped") return "端口映射";
    if (props.candidate.type === "stun") return "STUN 响应";
    if (props.candidate.type === "nic") return "网卡";
    return "未知";
  };
  return (
    <li>
      <code>{props.candidate.address}</code>
      <div class="address-origin"><b>{typeLabel()}</b><small>{props.candidate.interface || props.candidate.source || props.candidate.family || "系统"}</small></div>
    </li>
  );
}

function discoveryHintSourceLabel(source: string): string {
  return ({ "historical-remote": "历史远端", local: "本机", mdns: "mDNS", tracker: "Tracker", topology: "拓扑" } as Record<string, string>)[source] || "发现";
}

function formatDiscoveryHintExpiry(value: string | undefined, now: number): string {
  if (!value) return "会话期间有效";
  const target = Date.parse(value);
  if (!Number.isFinite(target)) return "有效期未知";
  const seconds = Math.max(0, Math.ceil((target-now) / 1000));
  return seconds === 0 ? "即将过期" : `线索剩余 ${seconds} 秒`;
}
