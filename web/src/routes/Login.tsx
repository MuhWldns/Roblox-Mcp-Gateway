import { useEffect, useState } from "react";
import { Navigate } from "react-router";
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
      <main>
        <p role="status">Loading…</p>
      </main>
    );
  }

  return (
    <main>
      <h1>Sign in to RobloxKit</h1>
      <p>
        RobloxKit connects the official Roblox Studio MCP to ChatGPT and Claude
        through your licensed Roblox account.
      </p>
      <button type="button" onClick={() => window.location.assign("/api/v1/auth/roblox/login")}>
        Login with Roblox
      </button>
    </main>
  );
}
