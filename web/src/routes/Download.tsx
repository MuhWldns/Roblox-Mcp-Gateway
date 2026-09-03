import { useEffect, useState } from "react";
import { Navigate } from "react-router";
import {
  type DownloadMetadata,
  type MeResponse,
  UnauthorizedError,
  getDownloadMetadata,
  getMe,
  logout,
} from "../api/client";

export default function Download() {
  const [me, setMe] = useState<MeResponse | null>(null);
  const [metadata, setMetadata] = useState<DownloadMetadata | null>(null);
  const [denied, setDenied] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    getMe()
      .then(async (profile) => {
        setMe(profile);
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

  if (denied) {
    return <Navigate to="/login" replace />;
  }
  if (failed && !me) {
    return (
      <main>
        <p role="alert">{failed}</p>
      </main>
    );
  }
  if (!me) {
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
      {me.trial?.active ? (
        <p data-testid="trial-state">
          Free trial active — ends {me.trial.ends_at.slice(0, 10)}
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
