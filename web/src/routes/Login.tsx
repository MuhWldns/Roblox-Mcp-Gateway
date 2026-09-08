import { useEffect, useRef, useState } from "react";
import { Link, Navigate, useLocation } from "react-router";
import { getMe } from "../api/client";

const maxReturnToLength = 4096;

function oauthAuthorizeContinuation(search: string): string {
  const candidate = new URLSearchParams(search).get("next") ?? "";
  if (
    candidate.length === 0 ||
    candidate.length > maxReturnToLength ||
    !candidate.startsWith("/") ||
    candidate.startsWith("//") ||
    candidate.includes("#")
  ) {
    return "";
  }
  try {
    const target = new URL(candidate, window.location.origin);
    if (
      target.origin !== window.location.origin ||
      target.username !== "" ||
      target.password !== "" ||
      target.pathname !== "/oauth/authorize"
    ) {
      return "";
    }
  } catch {
    return "";
  }
  return candidate;
}

export default function Login() {
  const location = useLocation();
  const returnTo = oauthAuthorizeContinuation(location.search);
  const resumeLink = useRef<HTMLAnchorElement>(null);
  const [authenticated, setAuthenticated] = useState<boolean | null>(null);

  useEffect(() => {
    let cancelled = false;
    getMe()
      .then(() => {
        if (!cancelled) setAuthenticated(true);
      })
      .catch(() => {
        if (!cancelled) setAuthenticated(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (authenticated === true && returnTo !== "") {
      resumeLink.current?.click();
    }
  }, [authenticated, returnTo]);

  if (authenticated === true) {
    if (returnTo === "") {
      return <Navigate to="/download" replace />;
    }
    return <a ref={resumeLink} href={returnTo} hidden aria-hidden="true" />;
  }
  if (authenticated === null) {
    return (
      <main className="flex items-center justify-center min-h-screen bg-navy p-4">
        <p role="status" className="text-white/70 text-sm">Checking your account…</p>
      </main>
    );
  }

  return (
    <main className="flex items-center justify-center min-h-screen bg-navy p-4">
      <div className="bg-white rounded-2xl shadow-xl border border-border p-8 sm:p-10 max-w-[420px] w-full text-center">
        <div className="w-12 h-12 rounded-xl bg-gradient-to-br from-red to-red-hover flex items-center justify-center font-extrabold text-white text-xl shadow-md mx-auto mb-4">
          B
        </div>
        <h1 className="text-2xl font-bold text-navy tracking-tight mb-1">Sign in to continue.</h1>
        <span className="inline-block text-[11px] font-bold text-text-muted uppercase tracking-wider mb-4 bg-surface-alt px-2 py-0.5 rounded border border-border">
          BY RBX ROYALE
        </span>
        <p className="text-sm text-text-secondary mb-6 leading-relaxed">
          {returnTo !== ""
            ? "Sign in with Roblox to review the requested access and continue connecting your AI assistant."
            : "Use your Roblox account to manage your computers and AI connections."}
        </p>
        <ol className="list-none p-0 m-0 mb-6 text-left space-y-2">
          {[
            "Sign in with Roblox",
            "Connect your computer and open Studio",
            "Connect ChatGPT or Claude",
          ].map((step, index) => (
            <li key={step} className="flex items-start gap-3 text-sm text-text-secondary">
              <span
                aria-hidden="true"
                className="shrink-0 w-6 h-6 rounded-full bg-navy text-white text-xs font-bold flex items-center justify-center"
              >
                {index + 1}
              </span>
              {step}
            </li>
          ))}
        </ol>
        <a
          href={
            returnTo === ""
              ? "/api/v1/auth/roblox/login"
              : `/api/v1/auth/roblox/login?${new URLSearchParams({ next: returnTo })}`
          }
          className="w-full px-6 py-3 text-base font-semibold bg-red text-white rounded-md hover:bg-red-hover transition-colors min-h-[44px] inline-flex items-center justify-center no-underline"
        >
          Continue with Roblox
        </a>
        <nav aria-label="Legal" className="flex justify-center gap-5 mt-6 text-xs text-text-secondary">
          <Link to="/privacy" className="hover:text-red underline underline-offset-4">Privacy Policy</Link>
          <Link to="/terms" className="hover:text-red underline underline-offset-4">Terms of Service</Link>
        </nav>
      </div>
    </main>
  );
}