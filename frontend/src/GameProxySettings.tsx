import * as Backend from "@wailsjs/go/app/App";
import { app } from "@wailsjs/go/models";
import { createEffect, createMemo, createSignal, For, Show } from "solid-js";
import type { ActionProps, AppState } from "./types";

interface GameProxySettingsProps {
  readonly gameProxy: AppState["gameProxy"];
  readonly busy: boolean;
  readonly runAction: ActionProps["runAction"];
}

interface GameProxyDraft {
  readonly directories: readonly string[];
  readonly server: string;
  readonly port: string;
  readonly username: string;
  readonly password: string;
  readonly mtu: string;
  readonly dns: string;
  readonly encrypt: boolean;
}

function draftFromConfig(config: app.GameProxyConfigInput): GameProxyDraft {
  return {
    directories: [...config.directories],
    server: config.node.server,
    port: String(config.node.port),
    username: config.node.username,
    password: config.node.password,
    mtu: String(config.node.mtu),
    dns: config.node.dns,
    encrypt: config.node.encrypt,
  };
}

function draftMatchesConfig(draft: GameProxyDraft, config: app.GameProxyConfigInput): boolean {
  return draft.directories.length === config.directories.length
    && draft.directories.every((directory, index) => directory === config.directories[index])
    && draft.server === config.node.server
    && draft.port === String(config.node.port)
    && draft.username === config.node.username
    && draft.password === config.node.password
    && draft.mtu === String(config.node.mtu)
    && draft.dns === config.node.dns
    && draft.encrypt === config.node.encrypt;
}

function boundedInteger(value: string, minimum: number, maximum: number): number | undefined {
  if (!/^\d+$/.test(value)) return undefined;
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed >= minimum && parsed <= maximum ? parsed : undefined;
}

function serializeDraft(draft: GameProxyDraft): app.GameProxyConfigInput | undefined {
  const port = boundedInteger(draft.port, 1, 65535);
  const mtu = boundedInteger(draft.mtu, 68, 1600);
  if (!draft.server.trim() || !draft.username.trim()
    || !draft.password || !draft.dns.trim() || port === undefined || mtu === undefined) return undefined;
  return new app.GameProxyConfigInput({
    directories: [...draft.directories],
    node: { server: draft.server, port, username: draft.username, password: draft.password, mtu, dns: draft.dns, encrypt: draft.encrypt },
  });
}

function configurableState(state: string): boolean {
  return state === "inactive" || state === "failed";
}

function activeState(state: string): boolean {
  return state === "starting" || state === "running" || state === "reconnecting" || state === "stopping";
}

function statusLabel(state: string): string {
  switch (state) {
    case "unsupported": return "当前系统不支持";
    case "inactive": return "未启动";
    case "starting": return "正在启动";
    case "running": return "运行中";
    case "reconnecting": return "正在重新连接";
    case "stopping": return "正在停止";
    case "failed": return "运行失败";
    default: return "未知状态";
  }
}

function statusTone(state: string): string {
  if (state === "running") return "running";
  if (state === "failed") return "failed";
  if (activeState(state)) return "transitioning";
  return "inactive";
}

function eventTime(value: string): string {
  const parsed = new Date(value);
  return Number.isNaN(parsed.valueOf()) ? value : parsed.toLocaleTimeString([], { hour12: false });
}

type LogFilter = "fatal" | "error" | "warning" | "info";
const logRanks: Record<LogFilter, number> = { fatal: 0, error: 1, warning: 2, info: 3 };

function formatBytes(value: number): string {
  if (value < 1024) return `${value} B`;
  const units = ["KiB", "MiB", "GiB"];
  let amount = value;
  let unit = "B";
  for (const next of units) {
    amount /= 1024;
    unit = next;
    if (amount < 1024 || next === units[units.length - 1]) break;
  }
  return `${amount.toFixed(amount >= 10 ? 1 : 2)} ${unit}`;
}

export function GameProxySettings(props: GameProxySettingsProps) {
  const [draft, setDraft] = createSignal(draftFromConfig(props.gameProxy.config));
  const [dirty, setDirty] = createSignal(false);
  const [logFilter, setLogFilter] = createSignal<LogFilter>("fatal");
  const serializedDraft = createMemo(() => serializeDraft(draft()));
  const status = () => props.gameProxy.status;
  const canConfigure = () => configurableState(status().state);
  const canUpdateDirectories = () => status().state === "running" || status().state === "reconnecting";
  const canSave = () => status().supported && (canConfigure() || canUpdateDirectories()) && dirty() && Boolean(serializedDraft()) && !props.busy;
  const canEditDirectories = () => status().supported && (canConfigure() || canUpdateDirectories()) && !props.busy;
  const canStart = () => status().supported && canConfigure() && !dirty() && draft().directories.length > 0 && Boolean(serializedDraft()) && !props.busy;
  const filteredEvents = createMemo(() => status().events.filter(
    (event) => (logRanks[event.level as LogFilter] ?? logRanks.info) <= logRanks[logFilter()],
  ));

  createEffect(() => {
    const config = props.gameProxy.config;
    if (!dirty()) setDraft(draftFromConfig(config));
  });

  function updateDraft(field: Exclude<keyof GameProxyDraft, "directories">, value: string) {
    const next = { ...draft(), [field]: value };
    setDraft(next);
    setDirty(!draftMatchesConfig(next, props.gameProxy.config));
  }

  function updateDirectories(directories: readonly string[]) {
    const next = { ...draft(), directories };
    setDraft(next);
    setDirty(!draftMatchesConfig(next, props.gameProxy.config));
  }

  function updateEncrypt(encrypt: boolean) {
    const next = { ...draft(), encrypt };
    setDraft(next);
    setDirty(!draftMatchesConfig(next, props.gameProxy.config));
  }

  async function selectDirectory() {
    let selected = "";
    const completed = await props.runAction(async () => {
      selected = await Backend.SelectGameProxyDirectory();
    }, { title: "选择游戏目录失败" });
    if (completed && selected && !draft().directories.includes(selected)) {
      updateDirectories([...draft().directories, selected]);
    }
  }

  async function saveConfig(event: SubmitEvent) {
    event.preventDefault();
    const input = serializedDraft();
    if (!input || !canSave()) return;
    if (await props.runAction(
      () => Backend.SaveGameProxyConfig(input),
      { title: "保存游戏代理设置失败" },
    )) setDirty(false);
  }

  return (
    <Show when={status().supported} fallback={
      <div class="diagnostic-section game-proxy-unsupported" aria-live="polite">
        <div class="diagnostic-heading"><span>可用性</span></div>
        <small class="empty-diagnostic">游戏代理仅在受支持的 Windows 版本中可用。</small>
      </div>
    }>
      <div class="game-proxy-settings">
        <form class="game-proxy-form" onSubmit={saveConfig}>
          <div class="game-proxy-field game-proxy-directory-field">
            <label>游戏目录</label>
            <div class="game-proxy-directory-list" aria-describedby="game-proxy-directory-help">
              <For each={draft().directories}>{(directory, index) => (
                <div class="game-proxy-directory-row">
                  <code title={directory}>{directory}</code>
                  <button class="push-to-talk-key-button" type="button" disabled={!canEditDirectories()} aria-label={`删除游戏目录 ${directory}`} onClick={() => updateDirectories(draft().directories.filter((_, itemIndex) => itemIndex !== index()))}>删除</button>
                </div>
              )}</For>
              <Show when={draft().directories.length === 0}>
                <small class="empty-diagnostic">尚未添加游戏目录。</small>
              </Show>
            </div>
            <button class="push-to-talk-key-button game-proxy-add-directory" type="button" disabled={!canEditDirectories()} onClick={() => void selectDirectory()}>添加目录</button>
            <small id="game-proxy-directory-help">选择一个或多个包含需要代理的游戏可执行文件的目录。</small>
          </div>
          <div class="game-proxy-node-grid">
            <div class="game-proxy-field game-proxy-field-wide">
              <label for="game-proxy-server">代理服务器</label>
              <input id="game-proxy-server" value={draft().server} required disabled={props.busy || activeState(status().state)} autocomplete="off" spellcheck={false} onInput={(event) => updateDraft("server", event.currentTarget.value)} />
            </div>
            <div class="game-proxy-field">
              <label for="game-proxy-port">端口</label>
              <input id="game-proxy-port" type="number" min="1" max="65535" step="1" value={draft().port} required disabled={props.busy || activeState(status().state)} inputmode="numeric" onInput={(event) => updateDraft("port", event.currentTarget.value)} />
            </div>
            <div class="game-proxy-field">
              <label for="game-proxy-mtu">MTU</label>
              <input id="game-proxy-mtu" type="number" min="68" max="1600" step="1" value={draft().mtu} required disabled={props.busy || activeState(status().state)} inputmode="numeric" onInput={(event) => updateDraft("mtu", event.currentTarget.value)} />
            </div>
            <div class="game-proxy-field">
              <label for="game-proxy-username">用户名</label>
              <input id="game-proxy-username" value={draft().username} required disabled={props.busy || activeState(status().state)} maxlength={253} autocomplete="username" spellcheck={false} onInput={(event) => updateDraft("username", event.currentTarget.value)} />
            </div>
            <div class="game-proxy-field">
              <label for="game-proxy-password">密码</label>
              <input id="game-proxy-password" type="password" value={draft().password} required disabled={props.busy || activeState(status().state)} maxlength={16} autocomplete="current-password" onInput={(event) => updateDraft("password", event.currentTarget.value)} />
            </div>
            <div class="game-proxy-field game-proxy-field-wide">
              <label for="game-proxy-dns">DNS IPv4 地址</label>
              <input id="game-proxy-dns" value={draft().dns} required disabled={props.busy || activeState(status().state)} inputmode="decimal" autocomplete="off" spellcheck={false} onInput={(event) => updateDraft("dns", event.currentTarget.value)} />
            </div>
            <label class="setting-row game-proxy-field-wide" for="game-proxy-encrypt">
              <small>数据混淆（XOR）</small>
              <input id="game-proxy-encrypt" type="checkbox" checked={draft().encrypt} disabled={props.busy || activeState(status().state)} onChange={(event) => updateEncrypt(event.currentTarget.checked)} />
            </label>
          </div>
          <div class="game-proxy-actions">
            <button class="push-to-talk-key-button" type="submit" disabled={!canSave()}>保存</button>
            <button class="push-to-talk-key-button" type="button" disabled={!canStart()} onClick={() => void props.runAction(Backend.StartGameProxy, { title: "启动游戏代理失败" })}>启动</button>
            <Show when={activeState(status().state)}>
              <button class="push-to-talk-key-button" type="button" disabled={props.busy || status().state === "stopping"} onClick={() => void props.runAction(Backend.StopGameProxy, { title: "停止游戏代理失败" })}>停止</button>
            </Show>
            <Show when={dirty()}><output>有未保存的更改。请先保存更改再启动。</output></Show>
          </div>
        </form>

        <div class="game-proxy-runtime" aria-live="polite">
          <div class="diagnostic-section">
            <div class="diagnostic-heading"><span>运行状态</span></div>
            <strong class={`game-proxy-status ${statusTone(status().state)}`}>{statusLabel(status().state)}</strong>
          </div>
          <div class="game-proxy-runtime-grid">
            <Show when={status().executableCount > 0}>
              <div class="diagnostic-section">
                <div class="diagnostic-heading"><span>可执行文件</span></div>
                <code class="diagnostic-value">{status().executableCount}</code>
              </div>
            </Show>
            <Show when={status().generation > 0}>
              <div class="diagnostic-section">
                <div class="diagnostic-heading"><span>运行代次</span></div>
                <code class="diagnostic-value">{status().generation}</code>
              </div>
            </Show>
          </div>
          <div class="diagnostic-section game-proxy-traffic">
            <div class="diagnostic-heading"><span>实时流量</span><b>每秒更新</b></div>
            <div class="game-proxy-traffic-grid">
              <div><small>上行</small><strong>{formatBytes(status().traffic.uploadRate)}/s</strong><code>总计 {formatBytes(status().traffic.uploadBytes)}</code></div>
              <div><small>下行</small><strong>{formatBytes(status().traffic.downloadRate)}/s</strong><code>总计 {formatBytes(status().traffic.downloadBytes)}</code></div>
            </div>
          </div>
          <Show when={activeState(status().state) && status().directories.length > 0}>
            <div class="diagnostic-section">
              <div class="diagnostic-heading"><span>本次运行目录</span></div>
              <ul class="game-proxy-running-directories">
                <For each={status().directories}>{(directory) => <li><code class="diagnostic-value">{directory}</code></li>}</For>
              </ul>
              <Show when={status().directories.length !== props.gameProxy.config.directories.length
                || status().directories.some((directory, index) => directory !== props.gameProxy.config.directories[index])}>
                <p class="diagnostic-note">当前进程仍使用与已保存设置不同的目录。</p>
              </Show>
            </div>
          </Show>
          <Show when={status().error}>
            <div class="diagnostic-section game-proxy-error">
              <div class="diagnostic-heading"><span>最近错误</span></div>
              <p class="diagnostic-note">{status().error}</p>
            </div>
          </Show>
          <Show when={status().events.length > 0}>
            <div class="diagnostic-section">
              <div class="diagnostic-heading"><span>连接日志</span><b>最近 {status().events.length} 条</b></div>
              <div class="game-proxy-log-toolbar">
                <label>显示等级<select value={logFilter()} onChange={(event) => setLogFilter(event.currentTarget.value as LogFilter)}><option value="fatal">Fatal</option><option value="error">Error+</option><option value="warning">Warning+</option><option value="info">全部</option></select></label>
                <button class="push-to-talk-key-button" type="button" disabled={props.busy} onClick={() => void props.runAction(Backend.ExportGameProxyLogs, { title: "导出游戏代理日志失败" })}>导出日志</button>
              </div>
              <ol class="game-proxy-log">
                <For each={filteredEvents()}>{(event) => (
                  <li class={event.level}>
                    <time datetime={event.at}>{eventTime(event.at)}</time>
                    <span>{event.message}</span>
                  </li>
                )}</For>
                <Show when={filteredEvents().length === 0}><li class="game-proxy-log-empty"><span>当前等级没有日志。</span></li></Show>
              </ol>
            </div>
          </Show>
        </div>
      </div>
    </Show>
  );
}
