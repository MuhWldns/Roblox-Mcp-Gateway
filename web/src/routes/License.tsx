import { useEffect, useState } from "react";
import { Link, Navigate } from "react-router";
import {
  type LicenseSnapshot,
  UnauthorizedError,
  getLicenseSnapshot,
  trialDaysRemaining,
} from "../api/client";

// License shows the trial window and paid-license slot state. It is strictly
// read-only: slot moves, Roblox identity rebinds, and license transfers are
// admin-only operations with no self-service control here.
export default function License() {
  const [snapshot, setSnapshot] = useState<LicenseSnapshot | null>(null);
  const [denied, setDenied] = useState(false);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    getLicenseSnapshot()
      .then((current) => {
        if (!cancelled) setSnapshot(current);
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

  const trial = snapshot?.license.trial ?? null;
  const license = snapshot?.license.license ?? null;
  const remaining =
    trial !== null && trial.active && snapshot !== null
      ? trialDaysRemaining(trial.ends_at, snapshot.clock)
      : 0;

  return (
    <section data-testid="page-license" aria-labelledby="license-title">
      <h2 id="license-title">License</h2>
      {failed ? (
        <p role="alert">License unavailable right now. Reload to try again.</p>
      ) : null}
      {snapshot === null && !failed ? <p role="status">Loading license…</p> : null}

      {snapshot !== null && trial !== null && trial.active ? (
        <section aria-label="Free trial">
          <h3>Free trial active</h3>
          <p data-testid="trial-window">
            Started {trial.started_at.slice(0, 10)} — ends {trial.ends_at.slice(0, 10)}
          </p>
          {remaining > 0 ? (
            <p data-testid="trial-remaining">
              {remaining === 1 ? "1 day" : `${remaining} days`} remaining
            </p>
          ) : null}
          <p>
            The trial runs for a fixed 14 × 24 hours and is never paused or
            reset — reinstalling, revoking devices, or account recovery do not
            extend it.
          </p>
        </section>
      ) : null}

      {snapshot !== null && trial !== null && !trial.active ? (
        <section aria-label="Expired free trial" data-testid="upgrade-cta">
          <h3>Your free trial has ended</h3>
          <p>
            Purchase a license to reconnect your devices. Paid licenses are
            provisioned by the RobloxKit team for your Roblox account and
            activate without reinstalling anything.
          </p>
          <p>
            <Link to="/download">Download the latest RobloxBridge</Link> for
            updates or recovery in the meantime.
          </p>
        </section>
      ) : null}

      {snapshot !== null && trial === null ? (
        <section aria-label="Trial not started">
          <h3>No free trial yet</h3>
          <p>
            Your one-time 14-day free trial starts only when your first device
            finishes enrollment. Logging in and downloading never start it.
          </p>
        </section>
      ) : null}

      {snapshot !== null ? (
        <section aria-label="Paid license">
          <h3>Paid license</h3>
          {license !== null ? (
            <p data-testid="license-state">
              Status: {license.status} · device slots: {license.device_slots} ·
              active bindings: {license.active_bindings}
            </p>
          ) : (
            <p>No paid license is active on this account yet.</p>
          )}
          <p>
            Slots bind to devices through enrollment or an audited admin
            transfer only — revoking a device never frees its slot, and
            Roblox-identity changes are handled by the support team.
          </p>
        </section>
      ) : null}
    </section>
  );
}
