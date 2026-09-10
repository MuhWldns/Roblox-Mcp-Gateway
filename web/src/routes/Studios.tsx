import { useCallback, useEffect, useRef, useState } from "react";
import { Navigate } from "react-router";
import { type StudioView, UnauthorizedError, getStudios } from "../api/client";
import StatusBadge from "../components/StatusBadge";

// Studios lists the Roblox Studio sessions the account's Bridges have
// reported, with their live lifecycle state as text.
export default function Studios() {
  const [studios, setStudios] = useState<StudioView[] | null>(null);
  const [denied, setDenied] = useState(false);
  const [failed, setFailed] = useState(false);
  const [refreshError, setRefreshError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);

  const inFlightRef = useRef(false);
  const timerRef = useRef<ReturnType<typeof window.setTimeout> | null>(null);
  const mountedRef = useRef(true);
  const deniedRef = useRef(false);
  const studiosRef = useRef<StudioView[] | null>(null);

  useEffect(() => {
    studiosRef.current = studios;
  }, [studios]);

  const scheduleNextRefresh = useCallback(() => {
    if (timerRef.current !== null) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
    if (
      !mountedRef.current ||
      deniedRef.current ||
      typeof document === "undefined" ||
      document.visibilityState !== "visible"
    ) {
      return;
    }
    timerRef.current = setTimeout(() => {
      void fetchStudios();
    }, 10000);
  }, []);

  const fetchStudios = useCallback(async () => {
    if (inFlightRef.current || !mountedRef.current || deniedRef.current) {
      return;
    }
    inFlightRef.current = true;
    setRefreshing(true);
    try {
      const list = await getStudios();
      if (!mountedRef.current) return;
      setStudios(list.studios);
      studiosRef.current = list.studios;
      setFailed(false);
      setRefreshError(null);
    } catch (error: unknown) {
      if (!mountedRef.current) return;
      if (error instanceof UnauthorizedError) {
        deniedRef.current = true;
        setDenied(true);
        if (timerRef.current !== null) {
          clearTimeout(timerRef.current);
          timerRef.current = null;
        }
        return;
      }
      if (studiosRef.current === null) {
        setFailed(true);
      } else {
        setRefreshError("Could not refresh Studio sessions. Showing last known state.");
      }
    } finally {
      if (mountedRef.current) {
        inFlightRef.current = false;
        setRefreshing(false);
        scheduleNextRefresh();
      }
    }
  }, [scheduleNextRefresh]);

  useEffect(() => {
    mountedRef.current = true;
    deniedRef.current = false;
    void fetchStudios();

    function onVisibilityChange() {
      if (document.visibilityState === "visible") {
        if (timerRef.current !== null) {
          clearTimeout(timerRef.current);
          timerRef.current = null;
        }
        void fetchStudios();
      } else {
        if (timerRef.current !== null) {
          clearTimeout(timerRef.current);
          timerRef.current = null;
        }
      }
    }

    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      mountedRef.current = false;
      document.removeEventListener("visibilitychange", onVisibilityChange);
      if (timerRef.current !== null) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
    };
  }, [fetchStudios]);

  if (denied) {
    return <Navigate to="/login" replace />;
  }

  return (
    <section
      data-testid="page-studios"
      aria-labelledby="studios-title"
      className="animate-[pageEnter_200ms_ease]"
    >
      <div className="flex flex-wrap items-center justify-between gap-4 mb-6">
        <div>
          <h2 id="studios-title" className="text-xl font-semibold text-navy mb-1">
            Studios
          </h2>
          <p className="text-text-secondary m-0">
            Roblox Studio sessions connected through your Bridges.
          </p>
        </div>
        <button
          type="button"
          onClick={() => {
            if (timerRef.current !== null) {
              clearTimeout(timerRef.current);
              timerRef.current = null;
            }
            void fetchStudios();
          }}
          disabled={refreshing}
          aria-busy={refreshing}
          className="px-3.5 py-1.5 text-sm font-medium border border-border rounded-md text-navy bg-white hover:bg-surface-alt transition-colors disabled:opacity-50 disabled:cursor-not-allowed inline-flex items-center gap-2"
        >
          {refreshing ? "Refreshing…" : "Refresh"}
        </button>
      </div>
      {failed ? (
        <div role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium mb-4 flex items-center justify-between gap-3">
          <span>Studios unavailable right now. Reload to try again.</span>
          <button
            type="button"
            onClick={() => {
              if (timerRef.current !== null) {
                clearTimeout(timerRef.current);
                timerRef.current = null;
              }
              void fetchStudios();
            }}
            className="text-xs font-semibold underline underline-offset-2 hover:text-red-hover"
          >
            Retry
          </button>
        </div>
      ) : null}
      {refreshError ? (
        <div role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium mb-4 flex items-center justify-between gap-3">
          <span>{refreshError}</span>
          <button
            type="button"
            onClick={() => {
              if (timerRef.current !== null) {
                clearTimeout(timerRef.current);
                timerRef.current = null;
              }
              void fetchStudios();
            }}
            className="text-xs font-semibold underline underline-offset-2 hover:text-red-hover"
          >
            Retry
          </button>
        </div>
      ) : null}
      {studios === null && !failed ? (
        <p role="status" className="text-text-muted italic">Loading Studio sessions…</p>
      ) : null}
      {studios !== null && studios.length === 0 ? (
        <div className="text-center py-12 px-6 bg-white border-2 border-dashed border-border rounded-lg">
          <p className="text-text-muted">
            No Studio sessions yet. Open Studio while Buildly companion is
            connected and a session will appear here automatically.
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
