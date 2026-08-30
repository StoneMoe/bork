import * as Backend from "@wailsjs/go/app/App";
import { app } from "@wailsjs/go/models";
import { createEffect, createMemo, createSignal, Show } from "solid-js";
import type { ActionProps, AppState } from "./types";

interface GameProxySettingsProps {
  readonly gameProxy: AppState["gameProxy"];
  readonly busy: boolean;
  readonly runAction: ActionProps["runAction"];
}

interface GameProxyDraft {
  readonly directory: string;
  readonly server: string;
  readonly port: string;
  readonly username: string;
  readonly password: string;
  readonly mtu: string;
  readonly dns: string;
}

function draftFromConfig(config: app.GameProxyConfigInput): GameProxyDraft {
  return {
    directory: config.directory,
    server: config.node.server,
    port: String(config.node.port),
    username: config.node.username,
    password: config.node.password,
    mtu: String(config.node.mtu),
    dns: config.node.dns,
  };
}

function draftMatchesConfig(draft: GameProxyDraft, config: app.GameProxyConfigInput): boolean {
  return draft.directory === config.directory
    && draft.server === config.node.server
    && draft.port === String(config.node.port)
    && draft.username === config.node.username
    && draft.password === config.node.password
    && draft.mtu === String(config.node.mtu)
    && draft.dns === config.node.dns;
}

function boundedInteger(value: string, minimum: number, maximum: number): number | undefined {
  if (!/^\d+$/.test(value)) return undefined;
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed >= minimum && parsed <= maximum ? parsed : undefined;
}

function serializeDraft(draft: GameProxyDraft): app.GameProxyConfigInput | undefined {
  const port = boundedInteger(draft.port, 1, 65535);
  const mtu = boundedInteger(draft.mtu, 46, 1600);
  if (!draft.directory.trim() || !draft.server.trim() || !draft.username.trim()
    || !draft.password || !draft.dns.trim() || port === undefined || mtu === undefined) return undefined;
  return new app.GameProxyConfigInput({
    directory: draft.directory,
    node: { server: draft.server, port, username: draft.username, password: draft.password, mtu, dns: draft.dns },
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

export function GameProxySettings(props: GameProxySettingsProps) {
  const [draft, setDraft] = createSignal(draftFromConfig(props.gameProxy.config));
  const [dirty, setDirty] = createSignal(false);
  const serializedDraft = createMemo(() => serializeDraft(draft()));
  const status = () => props.gameProxy.status;
  const canConfigure = () => configurableState(status().state);
  const canSave = () => status().supported && canConfigure() && dirty() && Boolean(serializedDraft()) && !props.busy;
  const canStart = () => status().supported && canConfigure() && !dirty() && Boolean(serializedDraft()) && !props.busy;

  createEffect(() => {
    const config = props.gameProxy.config;
    if (!dirty()) setDraft(draftFromConfig(config));
  });

  function updateDraft(field: keyof GameProxyDraft, value: string) {
    const next = { ...draft(), [field]: value };
    setDraft(next);
    setDirty(!draftMatchesConfig(next, props.gameProxy.config));
  }

  async function selectDirectory() {
    let selected = "";
    const completed = await props.runAction(async () => {
      selected = await Backend.SelectGameProxyDirectory();
    }, { title: "选择游戏目录失败" });
    if (completed && selected) updateDraft("directory", selected);
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
            <label for="game-proxy-directory">游戏目录</label>
            <div class="game-proxy-directory-control">
              <input id="game-proxy-directory" value={draft().directory} readonly required disabled={props.busy} aria-describedby="game-proxy-directory-help" />
              <button class="push-to-talk-key-button" type="button" disabled={props.busy} onClick={() => void selectDirectory()}>浏览</button>
            </div>
            <small id="game-proxy-directory-help">选择包含需要代理的游戏可执行文件的目录。</small>
          </div>
          <div class="game-proxy-node-grid">
            <div class="game-proxy-field game-proxy-field-wide">
              <label for="game-proxy-server">代理服务器</label>
              <input id="game-proxy-server" value={draft().server} required disabled={props.busy} autocomplete="off" spellcheck={false} onInput={(event) => updateDraft("server", event.currentTarget.value)} />
            </div>
            <div class="game-proxy-field">
              <label for="game-proxy-port">端口</label>
              <input id="game-proxy-port" type="number" min="1" max="65535" step="1" value={draft().port} required disabled={props.busy} inputmode="numeric" onInput={(event) => updateDraft("port", event.currentTarget.value)} />
            </div>
            <div class="game-proxy-field">
              <label for="game-proxy-mtu">MTU</label>
              <input id="game-proxy-mtu" type="number" min="46" max="1600" step="1" value={draft().mtu} required disabled={props.busy} inputmode="numeric" onInput={(event) => updateDraft("mtu", event.currentTarget.value)} />
            </div>
            <div class="game-proxy-field">
              <label for="game-proxy-username">用户名</label>
              <input id="game-proxy-username" value={draft().username} required disabled={props.busy} maxlength={253} autocomplete="username" spellcheck={false} onInput={(event) => updateDraft("username", event.currentTarget.value)} />
            </div>
            <div class="game-proxy-field">
              <label for="game-proxy-password">密码</label>
              <input id="game-proxy-password" type="password" value={draft().password} required disabled={props.busy} maxlength={16} autocomplete="current-password" onInput={(event) => updateDraft("password", event.currentTarget.value)} />
            </div>
            <div class="game-proxy-field game-proxy-field-wide">
              <label for="game-proxy-dns">DNS IPv4 地址</label>
              <input id="game-proxy-dns" value={draft().dns} required disabled={props.busy} inputmode="decimal" autocomplete="off" spellcheck={false} onInput={(event) => updateDraft("dns", event.currentTarget.value)} />
            </div>
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
          <Show when={status().directory}>
            <div class="diagnostic-section">
              <div class="diagnostic-heading"><span>当前运行目录</span></div>
              <code class="diagnostic-value">{status().directory}</code>
              <Show when={status().directory !== props.gameProxy.config.directory}>
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
        </div>
      </div>
    </Show>
  );
}
