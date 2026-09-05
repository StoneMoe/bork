import * as Backend from "@wailsjs/go/app/App";
import { For, Show, createMemo, createSignal } from "solid-js";
import type { IssueInput } from "./issues";
import type { AppState } from "./types";

type ProxyStatus = AppState["gameProxy"]["status"];
type Metric = "uploadRate" | "downloadRate" | "rttMs" | "lossPercent";

const connectionLabels: Record<string, string> = {
  inactive: "未加速",
  starting: "正在连接", running: "已连接", reconnecting: "正在重连",
  stopping: "正在停止", failed: "连接失败",
};

function rate(value: number): string {
  if (value < 1024) return `${Math.round(value)} B/s`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB/s`;
  if (value < 1024 ** 3) return `${(value / (1024 * 1024)).toFixed(1)} MiB/s`;
  return `${(value / 1024 ** 3).toFixed(1)} GiB/s`;
}

function plot(status: ProxyStatus, metric: Metric) {
  const end = Date.parse(status.quality.observedAt);
  const start = end - 180_000;
  const samples = (metric === "rttMs" || metric === "lossPercent"
    ? status.quality.history.map((sample) => ({ ...sample, value: sample[metric] }))
    : status.trafficHistory.map((sample) => ({ ...sample, value: sample[metric] }))).filter((sample) => {
    const at = Date.parse(sample.at);
    return at >= start && at <= end;
  });
  const speed = metric === "uploadRate" || metric === "downloadRate";
  const step = speed ? 1024 : 100;
  const maximum = metric === "lossPercent" ? 100 : Math.max(step,
    Math.ceil(Math.max(0, ...samples.map((sample) => sample.value ?? 0)) / step) * step);
  const points: { x: number; y: number }[] = [];
  const path: string[] = [];
  let previous: { at: number; generation: number } | undefined;
  for (const sample of samples) {
    const value = sample.value;
    if (value == null || !Number.isFinite(value)) {
      previous = undefined;
      continue;
    }
    const at = Date.parse(sample.at);
    const x = 2 + (at - start) / 180_000 * 176;
    const y = 32 - Math.max(0, Math.min(maximum, value)) / maximum * 28;
    // A timeout, reconnect, or unsampled interval is a gap, not a smooth line.
    const connected = previous && previous.generation === sample.generation && at - previous.at <= (speed ? 2500 : 4500);
    path.push(`${connected ? "L" : "M"}${x.toFixed(2)},${y.toFixed(2)}`);
    points.push({ x, y });
    previous = { at, generation: sample.generation };
  }
  return { path: path.join(" "), points, maximum };
}

function MetricChart(props: { status: ProxyStatus; metric: Metric; label: string }) {
  const chart = createMemo(() => plot(props.status, props.metric));
  const ended = () => props.status.state === "failed";
  const scale = () => props.metric === "rttMs" ? `${chart().maximum} ms`
    : props.metric === "lossPercent" ? `${chart().maximum}%` : rate(chart().maximum);
  return <div class="proxy-quality-chart">
    <svg viewBox="0 0 180 36" preserveAspectRatio="none" role="img"
      aria-label={`${props.label}${ended() ? "停止前" : "最近"} 3 分钟曲线，纵轴 0 至 ${scale()}；空白表示无有效测量`}>
      <path class="proxy-chart-grid" d="M2 4H178 M2 18H178 M2 32H178" />
      <path class="proxy-chart-line" d={chart().path} />
      <For each={chart().points}>{(point) => <circle cx={point.x} cy={point.y} r="1.15" />}</For>
    </svg>
    <Show when={chart().points.length === 0}><span class="proxy-chart-empty">暂无有效测量</span></Show>
    <div class="proxy-chart-axis"><span>3 分钟前</span><span title="纵轴满量程">{scale()}</span><span>{ended() ? "结束" : "现在"}</span></div>
  </div>;
}

export function GameProxyStatus(props: {
  status: ProxyStatus;
  nodeConfigured: boolean;
  hasDirectories: boolean;
  ready: boolean;
  refresh: () => Promise<void>;
  reportIssue: (issue: IssueInput) => void;
}) {
  const [pending, setPending] = createSignal(false);
  const connected = () => props.status.state === "running";
  const inactive = () => props.status.state === "inactive";
  const active = () => ["starting", "running", "reconnecting", "stopping"].includes(props.status.state);
  const canStart = () => props.nodeConfigured && props.hasDirectories && (inactive() || props.status.state === "failed");
  const quality = () => props.status.quality;
  const actionHelp = () => !active() && props.nodeConfigured && !props.hasDirectories ? "请先在设置中添加游戏目录"
    : props.status.state === "failed" ? "曲线已停止更新，详情见设置中的游戏代理" : "";

  async function toggleAcceleration() {
    if (!props.ready || pending() || props.status.state === "stopping" || (!active() && !canStart())) return;
    const start = !active();
    // Proxy commands must not hold the voice controls' global busy flag.
    setPending(true);
    try {
      await (start ? Backend.StartGameProxy() : Backend.StopGameProxy());
      await props.refresh();
    } catch (cause) {
      props.reportIssue({ type: "general", title: start ? "启动游戏代理失败" : "停止游戏代理失败",
        message: cause instanceof Error ? cause.message : String(cause || "未知错误") });
    } finally {
      setPending(false);
    }
  }

  return <footer class="game-proxy-footer" classList={{ "is-connected": connected(), "is-failed": props.status.state === "failed", "is-inactive": inactive() }} aria-label="游戏加速状态">
    <div class="proxy-connection">
      <strong title={connectionLabels[props.status.state] ?? "未连接"}>
        <i aria-hidden="true" />游戏加速
        <span class="visually-hidden" role="status">{connectionLabels[props.status.state] ?? "未连接"}</span>
      </strong>
      <Show when={actionHelp()}><small id="proxy-action-help">{actionHelp()}</small></Show>
      <Show when={active() || props.nodeConfigured}>
        <button class="push-to-talk-key-button proxy-action" type="button"
          classList={{ "is-start": !active() }}
          disabled={!props.ready || pending() || props.status.state === "stopping" || (!active() && !canStart())}
          aria-describedby={actionHelp() ? "proxy-action-help" : undefined}
          title={active() ? "停止游戏代理，不停止语音或卸载驱动" : "使用已保存的配置启动；驱动未准备好时可能请求 UAC 授权"}
          onClick={() => void toggleAcceleration()}>
          {pending() ? "正在处理" : props.status.state === "stopping" ? "正在停止" : active() ? "停止加速" : "开始加速"}
        </button>
      </Show>
    </div>
    <Show when={!inactive()}>
      <figure class="proxy-upload">
        <figcaption title="每秒采样的代理载荷上行速率"><span class="proxy-metric-label">上行速率</span><strong>{rate(connected() ? props.status.traffic.uploadRate : 0)}</strong></figcaption>
        <MetricChart status={props.status} metric="uploadRate" label="上行速率" />
      </figure>
      <figure class="proxy-download">
        <figcaption title="每秒采样的代理载荷下行速率"><span class="proxy-metric-label">下行速率</span><strong>{rate(connected() ? props.status.traffic.downloadRate : 0)}</strong></figcaption>
        <MetricChart status={props.status} metric="downloadRate" label="下行速率" />
      </figure>
      <figure class="proxy-latency">
        <figcaption title="到代理节点的往返延迟，每 2 秒探测一次，不代表游戏服务器延迟"><span class="proxy-metric-label">节点延迟</span><strong>{connected() && quality().rttMs != null ? `${quality().rttMs!.toFixed(1)} ms` : "--"}</strong></figcaption>
        <MetricChart status={props.status} metric="rttMs" label="节点延迟" />
      </figure>
      <figure class="proxy-loss">
        <figcaption title="最近 30 秒已完成探测中，2 秒内未应答的比例，不代表游戏数据包丢失率"><span class="proxy-metric-label">探测丢包</span><strong>{connected() && quality().lossPercent != null ? `${quality().lossPercent!.toFixed(1)}%` : "--"}</strong></figcaption>
        <MetricChart status={props.status} metric="lossPercent" label="探测丢包" />
      </figure>
    </Show>
  </footer>;
}
