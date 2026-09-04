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
    <section data-testid="page-studios" aria-labelledby="studios-title">
      <h2 id="studios-title">Studios</h2>
      <p>Roblox Studio sessions connected through your Bridges.</p>
      {failed ? <p role="alert">Studios unavailable right now. Reload to try again.</p> : null}
      {studios === null && !failed ? <p role="status">Loading Studio sessions…</p> : null}
      {studios !== null && studios.length === 0 ? (
        <p>
          No Studio sessions yet. Open Roblox Studio while RobloxBridge is
          connected and a session appears here.
        </p>
      ) : null}
      {studios !== null && studios.length > 0 ? (
        <ul>
          {studios.map((studio) => (
            <li key={studio.id} data-testid={`studio-${studio.id}`}>
              <h3>{studio.studio_id}</h3>
              <p>
                <StatusBadge status={studio.status} />
              </p>
              <p>
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
