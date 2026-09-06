import { useEffect, useState } from "react";
import { Link } from "react-router";
import { getMe } from "../api/client";
import Metadata from "../components/Metadata";

const steps = [
  {
    title: "Sign in with Roblox",
    body: "One click with your licensed Roblox account. No passwords stored, no separate signup.",
  },
  {
    title: "Connect your PC",
    body: "Install RobloxBridge and enter its pairing code on the dashboard. Outbound-only — no ports opened.",
  },
  {
    title: "Add the MCP connector",
    body: "Point ChatGPT or Claude at your RobloxKit connector and start scripting Studio with AI.",
  },
];

// Home is the public landing page. Signed-in visitors get a direct path to
// the dashboard; everyone else sees the sign-in CTA and the three-step flow.
export default function Home() {
  const [signedIn, setSignedIn] = useState(false);

  useEffect(() => {
    let cancelled = false;
    getMe()
      .then(() => {
        if (!cancelled) setSignedIn(true);
      })
      .catch(() => {
        if (!cancelled) setSignedIn(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <>
      <Metadata
        title="RobloxKit — ChatGPT & Claude for Roblox Studio"
        description="Connect the official Roblox Studio MCP to ChatGPT and Claude. Outbound-only Bridge, no port forwarding, 14-day free trial."
      />
      <div className="min-h-screen bg-surface flex flex-col">
        <header className="bg-white border-b border-border">
          <div className="max-w-[960px] mx-auto px-4 h-14 flex items-center justify-between">
            <p className="m-0 leading-none">
              <span className="text-lg font-bold text-navy tracking-tight align-middle">RobloxKit</span>
              <span className="ml-2 text-[11px] font-medium text-text-muted uppercase tracking-wider align-middle">
                BY RBX
              </span>
            </p>
            {signedIn ? (
              <Link
                to="/devices"
                className="text-sm font-semibold text-navy px-4 py-2 min-h-[36px] inline-flex items-center border border-border rounded-md bg-white hover:bg-surface-alt transition-colors no-underline"
              >
                Open dashboard
              </Link>
            ) : (
              <Link
                to="/login"
                className="text-sm font-semibold text-white px-4 py-2 min-h-[36px] inline-flex items-center bg-navy rounded-md hover:bg-navy-light transition-colors no-underline"
              >
                Sign in
              </Link>
            )}
          </div>
        </header>

        <main className="flex-1">
          <section className="max-w-[820px] mx-auto px-4 pt-16 pb-12 text-center max-md:pt-10">
            <h1 className="text-4xl font-bold text-navy tracking-tight mb-4 max-md:text-3xl">
              Control Roblox Studio from ChatGPT and Claude
            </h1>
            <p className="text-lg text-text-secondary mb-8 max-w-[640px] mx-auto">
              RobloxKit bridges the official Roblox Studio MCP to your AI
              assistant — safely, without port forwarding.
            </p>
            <div className="flex items-center justify-center gap-3 flex-wrap">
              {signedIn ? (
                <Link
                  to="/devices"
                  className="inline-flex items-center px-6 py-3 text-base font-semibold bg-red text-white rounded-md hover:bg-red-hover transition-colors no-underline min-h-[44px]"
                >
                  Open dashboard
                </Link>
              ) : (
                <Link
                  to="/login"
                  className="inline-flex items-center px-6 py-3 text-base font-semibold bg-red text-white rounded-md hover:bg-red-hover transition-colors no-underline min-h-[44px]"
                >
                  Sign in with Roblox
                </Link>
              )}
              <a
                href="#how"
                className="inline-flex items-center px-6 py-3 text-base font-semibold text-navy rounded-md border border-border bg-white hover:bg-surface-alt transition-colors no-underline min-h-[44px]"
              >
                See how it works
              </a>
            </div>
          </section>

          <section id="how" aria-label="How RobloxKit works" className="max-w-[960px] mx-auto px-4 pb-12">
            <ol className="list-none p-0 m-0 grid gap-4 sm:grid-cols-3">
              {steps.map((step, index) => (
                <li key={step.title} className="bg-white border border-border rounded-lg p-6">
                  <p
                    aria-hidden="true"
                    className="w-8 h-8 rounded-full bg-navy text-white text-sm font-bold flex items-center justify-center mb-3 m-0"
                  >
                    {index + 1}
                  </p>
                  <h2 className="text-base font-semibold text-navy mb-1">{step.title}</h2>
                  <p className="text-sm text-text-secondary m-0">{step.body}</p>
                </li>
              ))}
            </ol>
            <p className="text-center text-sm text-text-muted mt-8 mb-0">
              Outbound-only. RobloxBridge never opens ports on your PC.
            </p>
          </section>
        </main>

        <footer className="border-t border-border bg-white">
          <div className="max-w-[960px] mx-auto px-4 h-14 flex items-center justify-between text-xs text-text-secondary">
            <span>RobloxKit BY RBX</span>
            <nav aria-label="Legal" className="flex gap-5">
              <Link to="/privacy" className="hover:text-red underline underline-offset-4">Privacy Policy</Link>
              <Link to="/terms" className="hover:text-red underline underline-offset-4">Terms of Service</Link>
            </nav>
          </div>
        </footer>
      </div>
    </>
  );
}
