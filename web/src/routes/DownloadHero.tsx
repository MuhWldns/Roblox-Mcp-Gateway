import { useEffect, useState } from "react";
import { Navigate } from "react-router";
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
      <div role="alert" className="bg-red/10 text-red border border-red/20 rounded-xl p-5 text-sm font-medium">
        Release metadata unavailable right now. Please reload the page to try again.
      </div>
    );
  }
  if (snapshot === null || metadata === null) {
    return (
      <div className="py-20 flex flex-col items-center justify-center gap-3">
        <div className="w-8 h-8 rounded-full border-2 border-red border-t-transparent animate-spin" />
        <p role="status" className="text-white/60 text-sm font-mono tracking-wide">Loading release info…</p>
      </div>
    );
  }

  const trial = snapshot?.me.trial ?? null;
  const clock = snapshot?.clock ?? null;
  const remaining =
    trial !== null && clock !== null ? trialDaysRemaining(trial.ends_at, clock) : 0;

  return (
    <section aria-labelledby="download-hero-title" className="space-y-12">
      {/* Hero Header Section */}
      <div className="flex flex-col md:flex-row md:items-end justify-between gap-6 pb-8 border-b border-white/10">
        <div className="max-w-[640px]">
          <div className="inline-flex items-center gap-2 px-3 py-1 rounded-full bg-white/5 border border-white/10 text-white/80 text-xs font-semibold tracking-wide mb-4">
            <span className="w-2 h-2 rounded-full bg-red animate-pulse" />
            <span>Official MCP Bridge</span>
          </div>
          <h1 id="download-hero-title" className="text-3xl sm:text-5xl font-extrabold tracking-tight text-white mb-3">
            Download Buildly Companion
          </h1>
          <p className="text-base sm:text-lg text-white/70 leading-relaxed m-0">
            Connect your local Studio instance to ChatGPT and Claude over secure, outbound-only WebSockets.
          </p>
        </div>

        <div className="flex items-center gap-3 bg-white/5 border border-white/10 px-4 py-2.5 rounded-xl shrink-0">
          <div className="w-2.5 h-2.5 rounded-full bg-emerald-400 shadow-[0_0_8px_rgba(52,211,153,0.6)]" />
          <p className="text-xs text-white/80 m-0">
            Signed in as {snapshot.me.display_name}
          </p>
        </div>
      </div>

      {/* Split Grid: Download Card & Trial Status */}
      <div className="grid grid-cols-1 lg:grid-cols-12 gap-6 items-start">
        {/* Primary Download Box (7 cols) */}
        <div className="lg:col-span-7 bg-gradient-to-b from-[#131B2B] to-[#0E1524] border border-white/10 rounded-2xl p-6 sm:p-8 shadow-2xl relative overflow-hidden group">
          <div className="absolute top-0 right-0 w-80 h-80 bg-red/5 rounded-full blur-3xl pointer-events-none" />

          <div className="flex items-center justify-between gap-4 mb-6">
            <div className="flex items-center gap-2">
              <span className="px-3 py-1 rounded-md bg-white/10 text-white text-xs font-mono font-bold tracking-wider uppercase">
                Windows x64
              </span>
              <span className="text-xs text-white/60 font-mono">
                v<span data-testid="bridge-version">{metadata.version}</span>
              </span>
            </div>
            <span className="text-[11px] text-emerald-400 font-medium bg-emerald-400/10 border border-emerald-400/20 px-2.5 py-0.5 rounded-full">
              Verified Build
            </span>
          </div>

          <div className="mb-6">
            <div className="text-xl font-bold text-white font-mono tracking-tight mb-2 flex items-center gap-2">
              <svg className="w-5 h-5 text-red shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                <rect x="2" y="3" width="20" height="14" rx="2" ry="2" />
                <line x1="8" y1="21" x2="16" y2="21" />
                <line x1="12" y1="17" x2="12" y2="21" />
              </svg>
              <span>{metadata.filename}</span>
            </div>
            <div className="bg-[#080B11] border border-white/5 rounded-lg p-3 font-mono text-xs text-white/50 break-all select-all flex items-center justify-between gap-2">
              <span className="truncate">SHA-256: <span data-testid="bridge-checksum">{metadata.sha256}</span></span>
            </div>
          </div>

          <div className="space-y-3">
            <a
              data-testid="download-link"
              href="/api/v1/bridge/download"
              className="w-full inline-flex items-center justify-center gap-3 px-8 py-4 text-base font-bold bg-gradient-to-r from-red to-red-hover text-white rounded-xl hover:shadow-[0_0_24px_rgba(239,68,68,0.4)] transition-all transform active:scale-[0.99] no-underline"
            >
              <svg className="w-5 h-5 fill-current" viewBox="0 0 24 24">
                <path d="M19.35 10.04C18.67 6.59 15.64 4 12 4 9.11 4 6.6 5.64 5.35 8.04 2.34 8.36 0 10.91 0 14c0 3.31 2.69 6 6 6h13c2.76 0 5-2.24 5-5 0-2.64-2.05-4.78-4.65-4.96zM17 13l-5 5-5-5h3V9h4v4h3z" />
              </svg>
              <span>Download for Windows (.exe)</span>
            </a>
            <p className="text-center text-xs text-white/50 m-0">
              Zero admin rights required · Runs per-user · Outbound tunnel only
            </p>
          </div>
        </div>

        {/* Right Info Stack (5 cols) */}
        <div className="lg:col-span-5 space-y-4">
          {/* Trial Status Card */}
          {trial?.active ? (
            <div data-testid="trial-state" className="bg-[#121824] border border-red/30 rounded-2xl p-6 relative overflow-hidden">
              <div className="flex items-center justify-between gap-2 mb-3">
                <span className="text-[11px] font-bold text-red uppercase tracking-wider bg-red/10 border border-red/20 px-2.5 py-1 rounded">
                  Active Free Trial
                </span>
                <span data-testid="trial-countdown" className="text-xl font-extrabold text-white">
                  {remaining} days remaining
                </span>
              </div>
              <p className="text-xs text-white/60 m-0 font-mono">
                Valid: {trial.started_at.slice(0, 10)} → {trial.ends_at.slice(0, 10)}
              </p>
            </div>
          ) : null}

          {/* Trial Notice Banner */}
          <div data-testid="trial-notice" className="bg-[#101622] border border-white/10 rounded-2xl p-6">
            <div className="flex items-start gap-3">
              <div className="w-7 h-7 rounded-lg bg-red/10 text-red flex items-center justify-center font-bold text-sm shrink-0">
                🛡️
              </div>
              <div>
                <h4 className="text-sm font-bold text-white mb-1">Safe Evaluation</h4>
                <p className="text-xs text-white/70 leading-relaxed m-0">
                  Downloading does not start your free trial. Your 14-day trial begins
                  only when you connect and approve your first device.
                </p>
              </div>
            </div>
          </div>
        </div>
      </div>

      {/* Step Sequence Setup (Hallmark F4 Step Architecture) */}
      <div className="pt-8 border-t border-white/10">
        <div className="mb-6">
          <h3 className="text-xl font-bold text-white tracking-tight">
            Getting Started in 4 Steps
          </h3>
          <p className="text-sm text-white/60 m-0">
            Follow this simple walkthrough to pair your local Studio with Claude and ChatGPT.
          </p>
        </div>

        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
          <div className="bg-[#101622] border border-white/10 rounded-xl p-5 relative flex flex-col justify-between">
            <div>
              <div className="w-7 h-7 rounded-lg bg-white/10 text-white font-mono text-xs font-bold flex items-center justify-center mb-3">
                01
              </div>
              <h4 className="text-sm font-bold text-white mb-1.5">Run Executable</h4>
              <p className="text-xs text-white/60 leading-relaxed m-0">
                Double-click the downloaded executable to start the automatic smart setup wizard.
              </p>
            </div>
          </div>

          <div className="bg-[#101622] border border-white/10 rounded-xl p-5 relative flex flex-col justify-between">
            <div>
              <div className="w-7 h-7 rounded-lg bg-white/10 text-white font-mono text-xs font-bold flex items-center justify-center mb-3">
                02
              </div>
              <h4 className="text-sm font-bold text-white mb-1.5">Approve Browser</h4>
              <p className="text-xs text-white/60 leading-relaxed m-0">
                Your default browser will pop up to link this machine to your dashboard account.
              </p>
            </div>
          </div>

          <div className="bg-[#101622] border border-white/10 rounded-xl p-5 relative flex flex-col justify-between">
            <div>
              <div className="w-7 h-7 rounded-lg bg-white/10 text-white font-mono text-xs font-bold flex items-center justify-center mb-3">
                03
              </div>
              <h4 className="text-sm font-bold text-white mb-1.5">Launch Studio</h4>
              <p className="text-xs text-white/60 leading-relaxed m-0">
                Open your project in Studio. Buildly Companion detects and hooks your active instance.
              </p>
            </div>
          </div>

          <div className="bg-[#101622] border border-white/10 rounded-xl p-5 relative flex flex-col justify-between">
            <div>
              <div className="w-7 h-7 rounded-lg bg-white/10 text-white font-mono text-xs font-bold flex items-center justify-center mb-3">
                04
              </div>
              <h4 className="text-sm font-bold text-white mb-1.5">Connect AI</h4>
              <p className="text-xs text-white/60 leading-relaxed m-0">
                Add Buildly MCP server in Claude or ChatGPT to start building with AI in real-time.
              </p>
            </div>
          </div>
        </div>
      </div>
    </section>
  );
}