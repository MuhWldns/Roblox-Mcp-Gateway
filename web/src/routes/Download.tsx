import { useEffect, useState } from "react";
import { Navigate } from "react-router";
import {
  type DownloadMetadata,
  type MeSnapshot,
  UnauthorizedError,
  getDownloadMetadata,
  getMeSnapshot,
  logout,
  trialDaysRemaining,
} from "../api/client";

export default function Download() {
  const [snapshot, setSnapshot] = useState<MeSnapshot | null>(null);
  const [metadata, setMetadata] = useState<DownloadMetadata | null>(null);
  const [denied, setDenied] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);
  const [, setRefresh] = useState(0);

  useEffect(() => {
    let cancelled = false;
    getMeSnapshot()
      .then(async (current) => {
        setSnapshot(current);
        const meta = await getDownloadMetadata();
        if (!cancelled) setMetadata(meta);
      })
      .catch((error: unknown) => {
        if (error instanceof UnauthorizedError) {
          setDenied(true);
          return;
        }
        setFailed("Download information is unavailable right now.");
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const me = snapshot?.me ?? null;
  const clock = snapshot?.clock ?? null;
  const trial = me?.trial ?? null;

  // The countdown derives from the server clock snapshot on every render —
  // the client clock contributes only the elapsed delta since the response,
  // never the absolute "now". A missing server header degrades the anchor to
  // the client clock rather than hiding the trial state. The timer below only
  // forces periodic recomputation as time passes.
  const remaining =
    trial !== null && trial.active && clock !== null
      ? trialDaysRemaining(trial.ends_at, clock)
      : 0;

  useEffect(() => {
    if (trial === null || !trial.active || clock === null) {
      return;
    }
    const id = window.setInterval(() => setRefresh(Date.now()), 30_000);
    return () => window.clearInterval(id);
  }, [trial, clock]);

  if (denied) {
    return <Navigate to="/login" replace />;
  }
  if (failed && me === null) {
    return (
      <main>
        <p role="alert">{failed}</p>
      </main>
    );
  }
  if (me === null) {
    return (
      <main>
        <p role="status">Loading…</p>
      </main>
    );
  }
  return (
    <main>
      <h1>Download RobloxBridge</h1>
      <p>Signed in as {me.display_name}</p>

      {metadata ? (
        <dl>
          <dt>File</dt>
          <dd>{metadata.filename}</dd>
          <dt>Version</dt>
          <dd data-testid="bridge-version">{metadata.version}</dd>
          <dt>Checksum (SHA-256)</dt>
          <dd data-testid="bridge-checksum">{metadata.sha256}</dd>
          <dt>Size</dt>
          <dd>{Math.max(1, Math.round(metadata.size_bytes / 1024))} KB</dd>
        </dl>
      ) : (
        <p role="status">Loading download details…</p>
      )}

      <a data-testid="download-link" href="/api/v1/bridge/download">
        Download RobloxBridge.exe
      </a>
      <p data-testid="trial-notice">
        Downloading the Bridge does not start your free trial. Your 14-day trial
        begins only when your first device finishes enrollment.
      </p>
      {trial?.active ? (
        <p data-testid="trial-state">
          Free trial active — ends {trial.ends_at.slice(0, 10)}{" "}
          <span data-testid="trial-countdown">
            ({remaining === 1 ? "1 day" : `${remaining} days`} remaining)
          </span>
        </p>
      ) : null}
      <button
        type="button"
        onClick={() => {
          logout()
            .then(() => window.location.assign("/login"))
            .catch(() => window.location.assign("/login"));
        }}
      >
        Log out
      </button>
    </main>
  );
}
