import { useEffect, useState } from "react";
import { Navigate } from "react-router";
import { type DiagnosticsResponse, UnauthorizedError, getDiagnostics } from "../api/client";
import StatusBadge from "../components/StatusBadge";

// Diagnostics renders the service-side summary of the account's gateway
// resources. It is a read-only view: nothing here mutates state.
export default function Diagnostics() {
  const [summary, setSummary] = useState<DiagnosticsResponse | null>(null);
  const [denied, setDenied] = useState(false);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    getDiagnostics()
      .then((report) => {
        if (!cancelled) setSummary(report);
      })
      .catch((error: unknown) => {
        if (error instanceof UnauthorizedError) {
          setDenied(true);
          return;
        }
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (denied) {
    return <Navigate to="/login" replace />;
  }

  return (
    <section
      data-testid="page-diagnostics"
      aria-labelledby="diagnostics-title"
      className="animate-[pageEnter_200ms_ease]"
    >
      <h2 id="diagnostics-title" className="text-xl font-semibold text-navy mb-1">
        Diagnostics
      </h2>
      <p className="text-text-secondary mb-6">
        Service-side health of your account's gateway resources.
      </p>
      {failed ? (
        <div role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium mb-4">
          Diagnostics unavailable right now. Reload to try again.
        </div>
      ) : null}
      {summary === null && !failed ? (
        <p role="status" className="text-text-muted italic">Loading diagnostics…</p>
      ) : null}
      {summary !== null ? (
        <>
        <div className="grid grid-cols-[repeat(auto-fill,minmax(200px,1fr))] gap-4">
          <div className="bg-white border border-border rounded-lg p-5">
            <dt className="text-xs font-medium text-text-muted uppercase tracking-wider mb-1">
              Service database
            </dt>
            <dd className="text-2xl font-bold text-navy p-0" data-testid="diagnostics-database">
              {summary.database === "ok" ? <StatusBadge status="ok" /> : summary.database}
            </dd>
          </div>
          <div className="bg-white border border-border rounded-lg p-5">
            <dt className="text-xs font-medium text-text-muted uppercase tracking-wider mb-1">
              Devices registered
            </dt>
            <dd className="text-2xl font-bold text-navy p-0" data-testid="diagnostics-devices-registered">
              {summary.devices_registered}
            </dd>
          </div>
          <div className="bg-white border border-border rounded-lg p-5">
            <dt className="text-xs font-medium text-text-muted uppercase tracking-wider mb-1">
              Devices online
            </dt>
            <dd className="text-2xl font-bold text-navy p-0" data-testid="diagnostics-devices-online">
              {summary.devices_online}
            </dd>
          </div>
          <div className="bg-white border border-border rounded-lg p-5">
            <dt className="text-xs font-medium text-text-muted uppercase tracking-wider mb-1">
              Active Studio sessions
            </dt>
            <dd className="text-2xl font-bold text-navy p-0" data-testid="diagnostics-studios-active">
              {summary.studio_sessions_active}
            </dd>
          </div>
        </div>
        {summary.devices.length > 0 ? (
          <section aria-labelledby="diagnostic-devices-title" className="mt-6">
            <h3 id="diagnostic-devices-title" className="text-base font-semibold text-navy mb-3">
              Device operations
            </h3>
            <ul className="list-none p-0 m-0 grid gap-4">
              {summary.devices.map((device) => (
                <li
                  key={device.id}
                  data-testid={`diagnostic-device-${device.id}`}
                  className="bg-white border border-border rounded-lg p-5"
                >
                  <div className="flex items-start justify-between gap-4 mb-3">
                    <h4 className="text-base font-semibold text-navy m-0 break-words">{device.name}</h4>
                    <div className="flex flex-wrap justify-end gap-2">
                      <StatusBadge status={device.online ? "online" : "offline"} />
                      <StatusBadge status={device.status} />
                    </div>
                  </div>
                  <dl className="grid grid-cols-[minmax(0,10rem)_minmax(0,1fr)] gap-x-4 gap-y-2 text-sm max-sm:grid-cols-1">
                    <dt className="text-text-muted">Official MCP state</dt>
                    <dd className="text-navy break-words">
                      {device.official_mcp_state ?? "Unavailable"}
                    </dd>
                    <dt className="text-text-muted">Last heartbeat</dt>
                    <dd className="text-navy break-words">
                      {device.last_heartbeat_at?.slice(0, 19).replace("T", " ") ?? "Unavailable"}
                    </dd>
                    <dt className="text-text-muted">Reconnect count</dt>
                    <dd className="text-navy">{device.reconnect_count}</dd>
                    <dt className="text-text-muted">Last error</dt>
                    <dd className="text-navy break-words">{device.last_error ?? "None reported"}</dd>
                  </dl>
                </li>
              ))}
            </ul>
          </section>
        ) : null}
        </>
      ) : null}
    </section>
  );
}