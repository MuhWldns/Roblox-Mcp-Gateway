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
        <p role="status" className="text-white/70 text-sm">Loading…</p>
      </main>
    );
  }

  return (
    <main className="flex items-center justify-center min-h-screen bg-navy p-4">
      <div className="bg-white rounded-lg shadow-lg p-10 max-w-[420px] w-full text-center">
        <h1 className="text-2xl font-bold text-navy mb-3">Sign in to RobloxKit</h1>
        <p className="text-text-secondary mb-6">
          RobloxKit connects the official Roblox Studio MCP to ChatGPT and Claude
          through your licensed Roblox account.
        </p>
        <ol className="list-none p-0 m-0 mb-6 text-left space-y-2">
          {[
            "Sign in with Roblox",
            "Connect your PC by entering its pairing code",
            "Add the MCP connector in ChatGPT or Claude",
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