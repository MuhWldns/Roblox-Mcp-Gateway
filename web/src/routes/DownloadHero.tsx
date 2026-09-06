import { useEffect, useState } from "react";
import { Link, Navigate } from "react-router";
import {
  getDownloadMetadata,
  getMeSnapshot,
  trialDaysRemaining,
  type DownloadMetadata,
  type MeSnapshot,
  UnauthorizedError,
} from "../api/client";

// DownloadHero is the public download page that displays the current
// Bridge release version, download link, setup instructions, and trial
// status for authenticated visitors.
export default function DownloadHero() {
  const [metadata, setMetadata] = useState<DownloadMetadata | null>(null);
  const [snapshot, setSnapshot] = useState<MeSnapshot | null>(null);
  const [denied, setDenied] = useState(false);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    getMeSnapshot()
      .then(async (current) => {
        const release = await getDownloadMetadata();
        if (!cancelled) {
          setSnapshot(current);
          setMetadata(release);
        }
      })
      .catch((error: unknown) => {
        if (cancelled) return;
        if (error instanceof UnauthorizedError) {
          setDenied(true);
        } else {
          setFailed(true);
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (denied) {
    return <Navigate to="/login" replace />;
  }
  if (failed) {
    return (
      <p role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium">
        Release info unavailable right now. Reload to try again.
      </p>
    );
  }
  if (snapshot === null || metadata === null) {
    return <p role="status" className="text-text-muted italic">Loading release info…</p>;
  }

  const trial = snapshot?.me.trial ?? null;
  const clock = snapshot?.clock ?? null;
  const remaining =
    trial !== null && clock !== null ? trialDaysRemaining(trial.ends_at, clock) : 0;

  return (
    <section
      aria-labelledby="download-hero-title"
      className="animate-[pageEnter_200ms_ease]"
    >
      <h2 id="download-hero-title" className="text-xl font-semibold text-navy mb-1">
        Download RobloxBridge
      </h2>
      <p className="text-text-secondary mb-6">
        The desktop companion that connects your local Roblox Studio to the
        RobloxKit gateway, so ChatGPT and Claude can script your Studio through
        the official Roblox MCP.
      </p>

      <p className="text-text-secondary mb-4">
        Signed in as {snapshot.me.display_name}
      </p>

      {trial?.active ? (
        <div data-testid="trial-state" className="bg-white border border-border rounded-lg p-5 mb-4">
          <p className="font-semibold text-navy mb-1">Free trial active</p>
          <p className="text-sm text-text-secondary">
            {trial.started_at.slice(0, 10)} – {trial.ends_at.slice(0, 10)}
          </p>
          <p data-testid="trial-countdown" className="text-sm font-bold text-navy mt-1">
            {remaining} days remaining
          </p>
        </div>
      ) : null}
      <div className="bg-white border border-border rounded-lg p-6 mb-5 text-center">
        <p className="text-sm text-text-muted uppercase tracking-wider font-semibold mb-2">
          Latest version
        </p>
        <p data-testid="bridge-version" className="text-2xl font-bold text-navy mb-1">
          {metadata.version}
        </p>
        <p className="text-sm text-text-secondary mb-2">
          {metadata.filename}
        </p>
        <p data-testid="bridge-checksum" className="text-xs text-text-muted font-mono break-all mb-4">
          {metadata.sha256}
        </p>
        <a
          data-testid="download-link"
          href="/api/v1/bridge/download"
          className="inline-flex items-center px-6 py-3 text-sm font-semibold bg-red text-white rounded-md hover:bg-red-hover transition-colors no-underline"
        >
          Download for Windows
        </a>
      </div>

      <div data-testid="trial-notice" className="bg-white border border-border rounded-lg p-6 mb-5">
        <p className="text-sm text-text-secondary">
          Downloading does not start your free trial. Your 14-day trial begins
          only when you connect your first PC.
        </p>
      </div>

      <div className="bg-white border border-border rounded-lg p-6">
        <h3 className="text-base font-semibold text-navy mb-3">
          How to set up RobloxKit
        </h3>
        <ol className="list-decimal pl-5 space-y-4 text-sm text-text-secondary [&_li]:pl-1">
          <li>
            <strong className="text-navy">Download &amp; install RobloxBridge.</strong>{" "}
            The installer runs for your Windows user only — no administrator
            privileges needed.
          </li>
          <li>
            <strong className="text-navy">Sign in</strong> with your Roblox
            account on this dashboard, then{" "}
            <Link to="/devices" className="text-red hover:text-red-hover font-medium">
              connect your device
            </Link>
            . Connecting your PC starts your 14-day free trial.
          </li>
          <li>
            <strong className="text-navy">Open Roblox Studio</strong> while
            RobloxBridge is connected. The gateway detects the session
            automatically.
          </li>
          <li>
            <strong className="text-navy">Add the RobloxKit MCP server</strong>{" "}
            in ChatGPT or Claude.{" "}
            <Link to="/connectors" className="text-red hover:text-red-hover font-medium">
              Manage connectors
            </Link>{" "}
            from your dashboard.
          </li>
        </ol>
      </div>
    </section>
  );
}