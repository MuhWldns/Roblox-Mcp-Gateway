import { useEffect, useState } from "react";
import { Link, Navigate, useLocation } from "react-router";
import { UnauthorizedError } from "../api/client";
import { getSetupStatus, type SetupStatus } from "../api/setup";

export default function Setup() {
  const location = useLocation();
  const dashboard = location.pathname === "/dashboard";
  const [status, setStatus] = useState<SetupStatus | null>(null);
  const [error, setError] = useState("");
  const [denied, setDenied] = useState(false);
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    async function refresh() {
      try {
        const value = await getSetupStatus();
        if (!cancelled) { setStatus(value); setError(""); }
      } catch (err) {
        if (!cancelled) {
          if (err instanceof UnauthorizedError) setDenied(true);
          else setError("Could not refresh your connections. Check your connection and try again.");
        }
      } finally {
        if (!cancelled) timer = setTimeout(refresh, 10000);
      }
    }
    void refresh();
    return () => { cancelled = true; clearTimeout(timer); };
  }, [revision]);
  if (denied) return <Navigate to={`/login?${new URLSearchParams({ next: location.pathname })}`} replace />;
  const online = status?.devices.some(device => device.online) ?? false;
  const steps = [
    { title: "Connect your computer", complete: online, text: online ? "Companion is connected. Keep it running while you build." : status?.devices.length ? "Your computer is approved but offline. Open Buildly Companion on that computer." : "Download Companion on your Windows computer, run it, then approve the computer in your browser.", href: status?.devices.length ? "/devices" : "/download", action: status?.devices.length ? "Manage computers" : "Download Companion" },
    { title: "Open your Studio project", complete: !!status?.studios.length, text: status?.studios.length ? `${status.studios.length} Studio session(s) detected on your connected computers.` : "Open your project in Studio and leave Companion running. Your session will appear here when detected.", href: "/studios", action: "View Studio sessions" },
    { title: "Connect your AI assistant", complete: status?.ready ?? false, text: status?.ready ? "An authorized AI connection has an available Studio target. Check your license before making a tool request." : status?.connectors.length ? "Your AI assistant is authorized. Bring its computer online and open the selected project; choose a target if several Studios are open." : "Add Buildly to ChatGPT or Claude, then sign in and approve access. Authorization will appear here automatically.", href: status?.connectors.length ? "/connectors" : "/download#chatgpt-setup-title", action: status?.connectors.length ? "Manage AI connections" : "Open connection guide" },
  ];
  return <section aria-labelledby="setup-title" className="max-w-3xl">
    <div className="flex flex-wrap items-start justify-between gap-4 mb-8">
      <div><h2 id="setup-title" className="text-3xl font-bold text-navy mb-2">{dashboard ? "Your workspace" : "Set up Buildly"}</h2>
      <p className="text-text-secondary max-w-prose">{dashboard ? "Your computers, Studio sessions, and AI connections in one place." : "Connect your computer, open Studio, then give your AI assistant access."}</p></div>
      <Link to={dashboard ? "/setup" : "/dashboard"} className="text-sm font-semibold underline underline-offset-4">{dashboard ? "Setup guide" : "Explore dashboard"}</Link>
    </div>
    {error && <div role="alert" className="mb-6 text-red"><p>{error}</p><button type="button" onClick={() => setRevision(value => value + 1)} className="underline min-h-11">Try again</button></div>}
    {!status && !error && <p role="status">Checking your connections…</p>}
    {status && <>
      <p role="status" className="bg-surface-alt border border-border rounded-lg px-5 py-4 mb-6 font-semibold text-navy">{error ? "Showing last known status" : status.ready ? "Connected to Studio" : `${steps.filter(step => step.complete).length} of 3 connection steps ready`}</p>
      <ol className="list-none p-0 m-0 divide-y divide-border border-y border-border">
        {steps.map((step, index) => <li key={step.title} className="py-6 flex gap-4">
          <span className="shrink-0 w-8 h-8 rounded-full bg-navy text-white flex items-center justify-center font-semibold" aria-hidden="true">{index + 1}</span>
          <div className="min-w-0"><h3 className="text-lg font-semibold text-navy mb-1">{step.title}<span className="block sm:inline sm:ml-3 text-sm font-normal text-text-secondary">{step.complete ? "Connected" : "Action needed"}</span></h3><p className="text-text-secondary mb-3 max-w-prose">{step.text}</p><Link to={step.href} className="inline-flex items-center min-h-11 text-red font-semibold underline underline-offset-4">{step.action}</Link></div>
        </li>)}
      </ol>
      <div className="flex flex-wrap gap-5 mt-6 text-sm"><Link to="/license" className="underline min-h-11 inline-flex items-center">Check license and trial</Link><Link to="/diagnostics" className="underline min-h-11 inline-flex items-center">Troubleshoot a connection</Link>{!dashboard && status.ready && <Link to="/dashboard" className="bg-red text-white rounded-md px-5 min-h-11 inline-flex items-center font-semibold">Continue to dashboard</Link>}</div>
      <p className="text-xs text-text-secondary mt-4">Status refreshes every 10 seconds. An authorized connection does not guarantee an active license or a successful tool request.</p>
    </>}
  </section>;
}
