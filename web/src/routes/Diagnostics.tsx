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
    <section data-testid="page-diagnostics" aria-labelledby="diagnostics-title">
      <h2 id="diagnostics-title">Diagnostics</h2>
      <p>Service-side health of your account's gateway resources.</p>
      {failed ? (
        <p role="alert">Diagnostics unavailable right now. Reload to try again.</p>
      ) : null}
      {summary === null && !failed ? <p role="status">Loading diagnostics…</p> : null}
      {summary !== null ? (
        <dl>
          <dt>Service database</dt>
          <dd data-testid="diagnostics-database">
            {summary.database === "ok" ? <StatusBadge status="ok" /> : summary.database}
          </dd>
          <dt>Devices registered</dt>
          <dd data-testid="diagnostics-devices-registered">{summary.devices_registered}</dd>
          <dt>Devices online</dt>
          <dd data-testid="diagnostics-devices-online">{summary.devices_online}</dd>
          <dt>Active Studio sessions</dt>
          <dd data-testid="diagnostics-studios-active">{summary.studio_sessions_active}</dd>
        </dl>
      ) : null}
    </section>
  );
}
