import { useEffect, useState } from "react";
import { Navigate } from "react-router";
import { type StudioView, UnauthorizedError, getStudios } from "../api/client";
import StatusBadge from "../components/StatusBadge";

// Studios lists the Roblox Studio sessions the account's Bridges have
// reported, with their live lifecycle state as text.
export default function Studios() {
  const [studios, setStudios] = useState<StudioView[] | null>(null);
  const [denied, setDenied] = useState(false);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    getStudios()
      .then((list) => {
        if (!cancelled) setStudios(list.studios);
      })
      .catch((error: unknown) => {
        if (error instanceof UnauthorizedError) {
          setDenied(true);
          return;
        }
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (denied) {
    return <Navigate to="/login" replace />;
  }

  return (
    <section
      data-testid="page-studios"
      aria-labelledby="studios-title"
      className="animate-[pageEnter_200ms_ease]"
    >
      <h2 id="studios-title" className="text-xl font-semibold text-navy mb-1">
        Studios
      </h2>
      <p className="text-text-secondary mb-6">
        Roblox Studio sessions connected through your Bridges.
      </p>
      {failed ? (
        <div role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium mb-4">
          Studios unavailable right now. Reload to try again.
        </div>
      ) : null}
      {studios === null && !failed ? (
        <p role="status" className="text-text-muted italic">Loading Studio sessions…</p>
      ) : null}
      {studios !== null && studios.length === 0 ? (
        <div className="text-center py-12 px-6 bg-white border-2 border-dashed border-border rounded-lg">
          <p className="text-text-muted">
            No Studio sessions yet. Open Roblox Studio while RobloxBridge is
            connected and a session appears here.
          </p>
        </div>
      ) : null}
      {studios !== null && studios.length > 0 ? (
        <ul className="list-none p-0 m-0 grid gap-4">
          {studios.map((studio) => (
            <li
              key={studio.id}
              data-testid={`studio-${studio.id}`}
              className="bg-white border border-border rounded-lg p-5 shadow-sm hover:shadow-md transition-shadow"
            >
              <div className="flex items-start justify-between gap-4 mb-3">
                <h3 className="text-base font-semibold text-navy m-0">
                  {studio.studio_id}
                </h3>
                <StatusBadge status={studio.status} />
              </div>
              <p className="text-sm text-text-secondary">
                Started {studio.started_at.slice(0, 10)}
                {studio.ended_at !== null
                  ? ` · ended ${studio.ended_at.slice(0, 10)}`
                  : " · still running"}
              </p>
            </li>
          ))}
        </ul>
      ) : null}
    </section>
  );
}