import { useEffect, useState } from "react";
import { Link, Navigate } from "react-router";
import {
  type DeviceView,
  type StudioView,
  type ConnectorView,
  type MeSnapshot,
  UnauthorizedError,
  getDevices,
  getStudios,
  getConnectors,
  getMeSnapshot,
  trialDaysRemaining,
} from "../api/client";
import StatusBadge from "../components/StatusBadge";

export default function Dashboard() {
  const [devices, setDevices] = useState<DeviceView[] | null>(null);
  const [studios, setStudios] = useState<StudioView[] | null>(null);
  const [connectors, setConnectors] = useState<ConnectorView[] | null>(null);
  const [meSnapshot, setMeSnapshot] = useState<MeSnapshot | null>(null);
  const [denied, setDenied] = useState(false);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;

    async function loadData() {
      try {
        const [devRes, stuRes, conRes, meSnap] = await Promise.all([
          getDevices(),
          getStudios(),
          getConnectors(),
          getMeSnapshot(),
        ]);
        if (!cancelled) {
          setDevices(devRes.devices);
          setStudios(stuRes.studios);
          setConnectors(conRes.connectors);
          setMeSnapshot(meSnap);
          setError("");
        }
      } catch (err) {
        if (!cancelled) {
          if (err instanceof UnauthorizedError) setDenied(true);
          else setError("Could not load dashboard data. Check your connection.");
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    }

    void loadData();
    const interval = setInterval(loadData, 10000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, []);

  if (denied) {
    return <Navigate to="/login?next=%2Fdashboard" replace />;
  }

  const activeDevices = devices?.filter((d) => d.status === "active") ?? [];
  const onlineDevices = activeDevices.filter((d) => d.online);
  const activeStudios = studios?.filter((s) => s.status === "active" && !s.ended_at) ?? [];
  const activeConnectors = connectors?.filter((c) => !c.revoked_at) ?? [];

  const trial = meSnapshot?.me.trial;
  const daysLeft =
    trial?.active && trial.ends_at && meSnapshot
      ? trialDaysRemaining(trial.ends_at, meSnapshot.clock)
      : null;

  return (
    <section aria-labelledby="dashboard-title" className="animate-[pageEnter_200ms_ease] max-w-4xl">
      {/* Header */}
      <div className="flex flex-wrap items-center justify-between gap-4 mb-6">
        <div>
          <h2 id="dashboard-title" className="text-2xl font-bold text-navy mb-1">
            Dashboard
          </h2>
          <p className="text-sm text-text-secondary m-0">
            Real-time overview of your Buildly infrastructure and AI connections.
          </p>
        </div>
        <div className="flex gap-3">
          <Link
            to="/download"
            className="inline-flex items-center gap-2 px-4 py-2 text-sm font-semibold bg-navy text-white rounded-md hover:bg-navy-light transition-colors no-underline"
          >
            Download Companion
          </Link>
          <Link
            to="/setup"
            className="inline-flex items-center gap-2 px-4 py-2 text-sm font-semibold bg-red text-white rounded-md hover:bg-red-hover transition-colors no-underline"
          >
            Setup Guide
          </Link>
        </div>
      </div>

      {error && (
        <div role="alert" className="mb-6 p-4 bg-error-bg text-red border border-red/20 rounded-lg text-sm">
          {error}
        </div>
      )}

      {loading && !devices ? (
        <p role="status" className="text-sm text-text-secondary">
          Loading workspace status...
        </p>
      ) : (
        <>
          {/* Quick Metrics Grid */}
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 mb-8">
            <div className="bg-white border border-border rounded-xl p-5 shadow-xs">
              <div className="flex items-center justify-between mb-2">
                <span className="text-xs font-semibold uppercase tracking-wider text-text-muted">
                  Computers
                </span>
                <span
                  className={`w-2.5 h-2.5 rounded-full ${
                    onlineDevices.length > 0 ? "bg-emerald-500" : "bg-text-muted/40"
                  }`}
                />
              </div>
              <div className="text-2xl font-extrabold text-navy">
                {onlineDevices.length}{" "}
                <span className="text-xs font-normal text-text-muted">/ {activeDevices.length} online</span>
              </div>
              <Link to="/devices" className="text-xs text-red font-semibold hover:underline mt-2 inline-block">
                Manage computers &rarr;
              </Link>
            </div>

            <div className="bg-white border border-border rounded-xl p-5 shadow-xs">
              <div className="flex items-center justify-between mb-2">
                <span className="text-xs font-semibold uppercase tracking-wider text-text-muted">
                  Studios
                </span>
                <span
                  className={`w-2.5 h-2.5 rounded-full ${
                    activeStudios.length > 0 ? "bg-emerald-500" : "bg-text-muted/40"
                  }`}
                />
              </div>
              <div className="text-2xl font-extrabold text-navy">{activeStudios.length}</div>
              <Link to="/studios" className="text-xs text-red font-semibold hover:underline mt-2 inline-block">
                View Studio sessions &rarr;
              </Link>
            </div>

            <div className="bg-white border border-border rounded-xl p-5 shadow-xs">
              <div className="flex items-center justify-between mb-2">
                <span className="text-xs font-semibold uppercase tracking-wider text-text-muted">
                  AI Connectors
                </span>
                <span
                  className={`w-2.5 h-2.5 rounded-full ${
                    activeConnectors.length > 0 ? "bg-emerald-500" : "bg-text-muted/40"
                  }`}
                />
              </div>
              <div className="text-2xl font-extrabold text-navy">{activeConnectors.length}</div>
              <Link to="/connectors" className="text-xs text-red font-semibold hover:underline mt-2 inline-block">
                Configure AI &rarr;
              </Link>
            </div>

            <div className="bg-white border border-border rounded-xl p-5 shadow-xs">
              <div className="flex items-center justify-between mb-2">
                <span className="text-xs font-semibold uppercase tracking-wider text-text-muted">
                  License
                </span>
              </div>
              <div className="text-lg font-bold text-navy">
                {trial?.active ? (
                  <span className="text-emerald-600 font-extrabold">
                    {daysLeft !== null ? `${daysLeft} days trial` : "Trial Active"}
                  </span>
                ) : (
                  <span className="text-text-muted font-normal text-sm">Free / Inactive</span>
                )}
              </div>
              <Link to="/license" className="text-xs text-red font-semibold hover:underline mt-2 inline-block">
                License details &rarr;
              </Link>
            </div>
          </div>

          {/* Detailed Lists */}
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
            {/* Connected Computers Card */}
            <div className="bg-white border border-border rounded-xl p-6 shadow-xs">
              <div className="flex items-center justify-between mb-4">
                <h3 className="text-base font-bold text-navy m-0">Computers</h3>
                <Link to="/devices" className="text-xs text-red font-semibold hover:underline">
                  View all
                </Link>
              </div>
              {activeDevices.length === 0 ? (
                <div className="text-center py-6 border border-dashed border-border rounded-lg bg-surface-alt">
                  <p className="text-sm text-text-secondary mb-3">No computers connected yet.</p>
                  <Link
                    to="/download"
                    className="inline-flex items-center px-3 py-1.5 text-xs font-semibold bg-red text-white rounded-md no-underline hover:bg-red-hover"
                  >
                    Download Companion
                  </Link>
                </div>
              ) : (
                <ul className="list-none p-0 m-0 divide-y divide-border">
                  {activeDevices.slice(0, 4).map((device) => (
                    <li key={device.id} className="py-3 flex items-center justify-between">
                      <div>
                        <div className="text-sm font-semibold text-navy">{device.name}</div>
                        <div className="text-xs text-text-muted">
                          {device.hostname || "Unknown Host"} &middot; {device.platform || "Windows"}
                        </div>
                      </div>
                      <StatusBadge status={device.online ? "online" : "offline"} />
                    </li>
                  ))}
                </ul>
              )}
            </div>

            {/* AI Connectors Card */}
            <div className="bg-white border border-border rounded-xl p-6 shadow-xs">
              <div className="flex items-center justify-between mb-4">
                <h3 className="text-base font-bold text-navy m-0">AI Connectors</h3>
                <Link to="/connectors" className="text-xs text-red font-semibold hover:underline">
                  View all
                </Link>
              </div>
              {activeConnectors.length === 0 ? (
                <div className="text-center py-6 border border-dashed border-border rounded-lg bg-surface-alt">
                  <p className="text-sm text-text-secondary mb-3">No AI assistant authorized yet.</p>
                  <Link
                    to="/download#chatgpt-setup-title"
                    className="inline-flex items-center px-3 py-1.5 text-xs font-semibold bg-navy text-white rounded-md no-underline hover:bg-navy-light"
                  >
                    Connect ChatGPT / Claude
                  </Link>
                </div>
              ) : (
                <ul className="list-none p-0 m-0 divide-y divide-border">
                  {activeConnectors.slice(0, 4).map((connector) => (
                    <li key={connector.id} className="py-3 flex items-center justify-between">
                      <div>
                        <div className="text-sm font-semibold text-navy">{connector.client_name}</div>
                        <div className="text-xs text-text-muted">
                          {connector.scopes.join(", ")}
                        </div>
                      </div>
                      <span className="text-xs font-semibold text-emerald-600 bg-emerald-50 px-2 py-0.5 rounded">
                        Active
                      </span>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </div>
        </>
      )}
    </section>
  );
}
