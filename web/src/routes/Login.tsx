import { useEffect, useState } from "react";
import { Link, Navigate } from "react-router";
import { getMe } from "../api/client";

export default function Login() {
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

  if (authenticated === true) {
    return <Navigate to="/download" replace />;
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
        <button
          type="button"
          onClick={() => window.location.assign("/api/v1/auth/roblox/login")}
          className="w-full px-6 py-3 text-base font-semibold bg-red text-white rounded-md hover:bg-red-hover transition-colors min-h-[44px]"
        >
          Continue with Roblox
        </button>
        <nav aria-label="Legal" className="flex justify-center gap-5 mt-6 text-xs text-text-secondary">
          <Link to="/privacy" className="hover:text-red underline underline-offset-4">Privacy Policy</Link>
          <Link to="/terms" className="hover:text-red underline underline-offset-4">Terms of Service</Link>
        </nav>
      </div>
    </main>
  );
}