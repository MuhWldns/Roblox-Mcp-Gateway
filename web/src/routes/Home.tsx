import { useEffect, useState } from "react";
import { Link } from "react-router";
import { getMe } from "../api/client";
import Metadata from "../components/Metadata";

const steps = [
  {
    title: "Sign in to Buildly",
    body: "Use your Roblox account to manage your computers and AI connections.",
  },
  {
    title: "Connect your computer",
    body: "Download and run Buildly Companion on your Windows PC, approve the connection, then open your Studio project.",
  },
  {
    title: "Connect your AI assistant",
    body: "Add Buildly to ChatGPT or Claude. Start by asking your assistant to read your project without making changes.",
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
        title="Buildly by RBX Royale — Bring your ideas to Studio, with AI"
        description="Connect ChatGPT or Claude to your Studio project. Get help understanding your project, writing scripts, and making changes with AI."
      />
      <div className="min-h-screen bg-surface flex flex-col">
        <header className="bg-white border-b border-border">
          <div className="max-w-[960px] mx-auto px-4 h-14 flex items-center justify-between">
            <div className="flex items-center gap-2">
              <div className="w-8 h-8 rounded-lg bg-gradient-to-br from-red to-red-hover flex items-center justify-center font-bold text-white shadow-sm">
                B
              </div>
              <div>
                <span className="text-lg font-bold text-navy tracking-tight align-middle">Buildly</span>
                <span className="ml-2 text-[10px] font-bold text-text-muted uppercase tracking-wider align-middle bg-surface-alt px-1.5 py-0.5 rounded border border-border">
                  BY RBX ROYALE
                </span>
              </div>
            </div>
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
            <h1 className="text-4xl sm:text-5xl font-extrabold text-navy tracking-tight mb-4">
              Bring your ideas to Studio, <span className="text-red">with AI.</span>
            </h1>
            <p className="text-lg text-text-secondary mb-8 max-w-[640px] mx-auto">
              Connect ChatGPT or Claude to your Studio project. Get help understanding
              your project, writing scripts, and making changes with AI.
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
                  Start setup
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

          <section id="how" aria-label="How Buildly works" className="max-w-[960px] mx-auto px-4 pb-12">
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
              You’ll need a Windows PC with Studio and access to custom apps or connectors in ChatGPT or Claude.
            </p>
          </section>
        </main>

        <footer className="border-t border-border bg-white">
          <div className="max-w-[960px] mx-auto px-4 h-14 flex items-center justify-between text-xs text-text-secondary">
            <span>Buildly by RBX Royale</span>
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
