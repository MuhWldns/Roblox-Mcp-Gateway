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
      className="animate-[pageEnter_200ms_ease] max-w-[840px]"
    >
      {/* Header Banner */}
      <div className="mb-8">
        <div className="inline-flex items-center gap-2 px-2.5 py-1 rounded-full bg-red/10 text-red text-xs font-semibold uppercase tracking-wider mb-3">
          <span>⚡ Desktop Companion</span>
        </div>
        <h2 id="download-hero-title" className="text-3xl font-extrabold text-navy tracking-tight mb-2">
          Download Buildly Companion
        </h2>
        <p className="text-base text-text-secondary leading-relaxed mb-3">
          The high-performance desktop bridge that connects your local Studio instance
          to the Buildly MCP Gateway, enabling real-time AI assistance from Claude and ChatGPT.
        </p>
        <p className="text-xs text-text-muted flex items-center gap-2 m-0">
          <span className="w-2 h-2 rounded-full bg-green-500 inline-block"></span>
          <span>Signed in as {snapshot.me.display_name}</span>
        </p>
      </div>

      {/* Trial Status Card */}
      {trial?.active ? (
        <div data-testid="trial-state" className="bg-gradient-to-r from-navy to-navy-light text-white rounded-xl p-5 mb-6 shadow-sm flex items-center justify-between gap-4 max-sm:flex-col max-sm:items-start">
          <div>
            <div className="flex items-center gap-2 mb-1">
              <span className="text-xs font-semibold uppercase tracking-wider text-red bg-red/20 px-2 py-0.5 rounded">Active Trial</span>
              <span className="text-xs text-white/70">14-Day Free Access</span>
            </div>
            <p className="text-sm text-white/80 m-0">
              {trial.started_at.slice(0, 10)} – {trial.ends_at.slice(0, 10)}
            </p>
          </div>
          <div className="text-right max-sm:text-left">
            <span data-testid="trial-countdown" className="text-2xl font-black text-white block">
              {remaining} days remaining
            </span>
          </div>
        </div>
      ) : null}

      {/* Primary Download Card */}
      <div className="bg-white border border-border rounded-2xl p-8 mb-6 shadow-sm hover:shadow-md transition-shadow">
        <div className="grid grid-cols-1 md:grid-cols-[1fr_auto] gap-6 items-center">
          <div>
            <div className="flex items-center gap-3 mb-2">
              <span className="text-xs font-bold text-text-muted uppercase tracking-wider bg-surface-alt px-2.5 py-1 rounded border border-border">
                Windows x64
              </span>
              <span className="text-sm font-bold text-navy">
                v<span data-testid="bridge-version">{metadata.version}</span>
              </span>
            </div>
            <h3 className="text-xl font-bold text-navy mb-1">
              {metadata.filename}
            </h3>
            <p className="text-xs text-text-muted font-mono break-all mb-0">
              SHA-256: <span data-testid="bridge-checksum">{metadata.sha256}</span>
            </p>
          </div>

          <div className="flex flex-col items-stretch md:items-end gap-2">
            <a
              data-testid="download-link"
              href="/api/v1/bridge/download"
              className="inline-flex items-center justify-center gap-2 px-8 py-3.5 text-base font-bold bg-red text-white rounded-xl hover:bg-red-hover hover:scale-[1.02] active:scale-[0.98] transition-all shadow-md no-underline"
            >
              <svg className="w-5 h-5 fill-current" viewBox="0 0 24 24">
                <path d="M19.35 10.04C18.67 6.59 15.64 4 12 4 9.11 4 6.6 5.64 5.35 8.04 2.34 8.36 0 10.91 0 14c0 3.31 2.69 6 6 6h13c2.76 0 5-2.24 5-5 0-2.64-2.05-4.78-4.65-4.96zM17 13l-5 5-5-5h3V9h4v4h3z" />
              </svg>
              <span>Download for Windows</span>
            </a>
            <span className="text-[11px] text-text-muted text-center md:text-right">
              Runs per-user · No admin privileges required
            </span>
          </div>
        </div>
      </div>

      {/* Trial Notice Badge */}
      <div data-testid="trial-notice" className="bg-surface-alt border border-border rounded-xl p-4 mb-8 flex items-center gap-3 text-sm text-text-secondary">
        <div className="w-8 h-8 rounded-lg bg-navy/5 flex items-center justify-center text-navy shrink-0 font-bold">
          💡
        </div>
        <p className="m-0 leading-relaxed text-xs sm:text-sm">
          <strong>Notice:</strong> Downloading does not start your free trial. Your 14-day trial begins
          only when you connect and approve your first device.
        </p>
      </div>

      {/* Quick Setup Guide */}
      <div className="bg-white border border-border rounded-2xl p-6 sm:p-8">
        <h3 className="text-lg font-bold text-navy mb-4 flex items-center gap-2">
          <span>🚀 Quick Setup Guide</span>
        </h3>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <div className="p-4 rounded-xl bg-surface border border-border">
            <div className="w-6 h-6 rounded-full bg-navy text-white text-xs font-bold flex items-center justify-center mb-2">1</div>
            <h4 className="text-sm font-bold text-navy mb-1">Install &amp; Run</h4>
            <p className="text-xs text-text-secondary leading-relaxed m-0">
              Launch the downloaded executable. It will guide you through terminal setup and pairing.
            </p>
          </div>

          <div className="p-4 rounded-xl bg-surface border border-border">
            <div className="w-6 h-6 rounded-full bg-navy text-white text-xs font-bold flex items-center justify-center mb-2">2</div>
            <h4 className="text-sm font-bold text-navy mb-1">Approve Device</h4>
            <p className="text-xs text-text-secondary leading-relaxed m-0">
              Your browser opens automatically to link this machine. Or manage it via{" "}
              <Link to="/devices" className="text-red hover:underline font-semibold">Devices</Link>.
            </p>
          </div>

          <div className="p-4 rounded-xl bg-surface border border-border">
            <div className="w-6 h-6 rounded-full bg-navy text-white text-xs font-bold flex items-center justify-center mb-2">3</div>
            <h4 className="text-sm font-bold text-navy mb-1">Open Studio</h4>
            <p className="text-xs text-text-secondary leading-relaxed m-0">
              Start your Studio session. Buildly detects running instances in real-time.
            </p>
          </div>

          <div className="p-4 rounded-xl bg-surface border border-border">
            <div className="w-6 h-6 rounded-full bg-navy text-white text-xs font-bold flex items-center justify-center mb-2">4</div>
            <h4 className="text-sm font-bold text-navy mb-1">Connect AI</h4>
            <p className="text-xs text-text-secondary leading-relaxed m-0">
              Add the Buildly MCP server in Claude or ChatGPT under{" "}
              <Link to="/connectors" className="text-red hover:underline font-semibold">Connectors</Link>.
            </p>
          </div>
        </div>
      </div>
    </section>
  );
}