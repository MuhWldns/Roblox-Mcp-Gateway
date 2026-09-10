import { useCallback, useEffect, useRef, useState } from "react";
import { Navigate } from "react-router";
import { ApiError, UnauthorizedError, getAdminMetrics, type AdminMetricsResponse } from "../api/client";

const refreshIntervalMs = 5_000;

function formatDuration(seconds: number) {
  const days = Math.floor(seconds / 86_400);
  const hours = Math.floor((seconds % 86_400) / 3_600);
  const minutes = Math.floor((seconds % 3_600) / 60);
  return [days ? `${days}d` : "", hours ? `${hours}h` : "", `${minutes}m`].filter(Boolean).join(" ");
}

function MetricCard({ label, value, detail, alert = false }: { label: string; value: string | number; detail: string; alert?: boolean }) {
  return <div className={`rounded-lg border p-5 ${alert ? "border-warning bg-warning-bg" : "border-border bg-white"}`}>
    <dt className="mb-2 text-xs font-semibold uppercase tracking-wider text-text-muted">{label}</dt>
    <dd className="m-0 text-3xl font-bold tabular-nums text-navy">{value}</dd>
    <p className="mb-0 mt-2 text-sm text-text-secondary">{detail}</p>
  </div>;
}

export default function AdminMetrics() {
  const [snapshot, setSnapshot] = useState<AdminMetricsResponse | null>(null);
  const [error, setError] = useState("");
  const [denied, setDenied] = useState(false);
  const running = useRef(false);

  const load = useCallback(async () => {
    if (running.current) return;
    running.current = true;
    try {
      setSnapshot(await getAdminMetrics());
      setError("");
    } catch (cause) {
      if (cause instanceof UnauthorizedError) setDenied(true);
      else setError(cause instanceof ApiError && cause.status === 403 ? "Administrator access required." : "Metrics unavailable. Try again.");
    } finally {
      running.current = false;
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") void load();
    }, refreshIntervalMs);
    return () => window.clearInterval(timer);
  }, [load]);

  if (denied) return <Navigate to="/login" replace />;
  const auditPercent = snapshot && snapshot.audit.queue_capacity > 0
    ? Math.round(snapshot.audit.queue_depth / snapshot.audit.queue_capacity * 100)
    : 0;
  const unhealthyAudit = Boolean(snapshot && (snapshot.audit.dropped > 0 || auditPercent >= 80));

  return <section data-testid="page-admin-metrics" aria-labelledby="admin-metrics-title" className="animate-[pageEnter_200ms_ease]">
    <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <p className="mb-1 text-xs font-semibold uppercase tracking-[0.14em] text-red">Operations</p>
        <h2 id="admin-metrics-title" className="mb-1 text-xl font-semibold text-navy">Live gateway metrics</h2>
        <p className="m-0 max-w-[70ch] text-text-secondary">Process-local load and saturation. Refreshes every five seconds while this tab is visible.</p>
      </div>
      <button type="button" onClick={() => void load()} className="min-h-10 rounded-md border border-border bg-white px-4 text-sm font-semibold text-navy hover:bg-surface-alt">Refresh now</button>
    </div>
    {error && <div role="alert" className="mb-4 flex items-center justify-between gap-4 rounded-md border border-red bg-error-bg px-4 py-3 text-sm font-medium text-red"><span>{error}</span><button type="button" onClick={() => void load()} className="underline">Retry</button></div>}
    {!snapshot && !error && <p role="status" className="text-text-muted italic">Loading metrics…</p>}
    {snapshot && <>
      <dl className="grid grid-cols-[repeat(auto-fit,minmax(190px,1fr))] gap-4">
        <MetricCard label="Uptime" value={formatDuration(snapshot.uptime_seconds)} detail="Since this server process started" />
        <MetricCard label="Bridges online" value={snapshot.bridge.online} detail={`${snapshot.bridge.reconnects} reconnects this process`} />
        <MetricCard label="MCP in flight" value={snapshot.mcp.inflight} detail={`${snapshot.mcp.total_requests} calls observed`} />
        <MetricCard label="Audit queue" value={`${auditPercent}%`} detail={`${snapshot.audit.queue_depth} of ${snapshot.audit.queue_capacity} events buffered`} alert={unhealthyAudit} />
      </dl>
      <div className="mt-6 grid gap-4 lg:grid-cols-2">
        <section aria-labelledby="latency-title" className="rounded-lg border border-border bg-white p-5">
          <h3 id="latency-title" className="mb-4 text-base font-semibold text-navy">MCP latency</h3>
          <dl className="grid grid-cols-3 gap-3">
            {([['p50', snapshot.mcp.latency_ms.p50], ['p95', snapshot.mcp.latency_ms.p95], ['p99', snapshot.mcp.latency_ms.p99]] as const).map(([label, value]) => <div key={label}><dt className="text-xs font-semibold uppercase text-text-muted">{label}</dt><dd className="mt-1 font-mono text-xl font-semibold text-navy">{value.toFixed(1)} ms</dd></div>)}
          </dl>
        </section>
        <section aria-labelledby="pressure-title" className="rounded-lg border border-border bg-white p-5">
          <h3 id="pressure-title" className="mb-4 text-base font-semibold text-navy">Pressure signals</h3>
          <dl className="grid gap-3 text-sm">
            <div className="flex justify-between gap-4"><dt className="text-text-secondary">Slow bridge consumers dropped</dt><dd className="font-mono font-semibold text-navy">{snapshot.bridge.slow_consumer_drops}</dd></div>
            <div className="flex justify-between gap-4"><dt className="text-text-secondary">Success audit events dropped</dt><dd className={`font-mono font-semibold ${snapshot.audit.dropped ? "text-red" : "text-success"}`}>{snapshot.audit.dropped}</dd></div>
          </dl>
        </section>
      </div>
    </>}
  </section>;
}
