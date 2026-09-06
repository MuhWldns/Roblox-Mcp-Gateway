import { useEffect, useState } from "react";
import { Link, Navigate } from "react-router";
import {
  type LicenseSnapshot,
  type LicenseState,
  type TrialState,
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
        if (cancelled) return;
        if (error instanceof UnauthorizedError) {
          setDenied(true);
        } else {
          setFailed(true);
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (denied) {
    return <Navigate to="/login" replace />;
  }

  const trial: TrialState | null = snapshot?.license.trial ?? null;
  const license: LicenseState | null = snapshot?.license.license ?? null;
  const remaining =
    trial !== null && trial.active && snapshot !== null
      ? trialDaysRemaining(trial.ends_at, snapshot.clock)
      : 0;

  return (
    <section
      data-testid="page-license"
      aria-labelledby="license-title"
      className="animate-[pageEnter_200ms_ease]"
    >
      <h2 id="license-title" className="text-xl font-semibold text-navy mb-1">
        License
      </h2>

      {failed ? (
        <div role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium mb-4">
          License unavailable right now. Reload to try again.
        </div>
      ) : null}

      {snapshot === null && !failed ? (
        <p role="status" className="text-text-muted italic">Loading license…</p>
      ) : null}

      {/* Active trial */}
      {snapshot !== null && trial !== null && trial.active ? (
        <section aria-label="Free trial" className="bg-white border border-border rounded-lg p-5 mb-4">
          <h3 className="text-sm font-semibold text-text-muted uppercase tracking-wider mb-3">
            Free trial
          </h3>
          <p className="text-base font-semibold text-navy mb-1">Free trial active</p>
          <div
            data-testid="trial-window"
            className="text-[13px] text-text-muted mb-2"
          >
            {trial.started_at.slice(0, 10)} – {trial.ends_at.slice(0, 10)}
          </div>
          <div
            data-testid="trial-remaining"
            className={`text-4xl font-bold leading-none mb-1 ${
              remaining <= 3 ? "text-warning" : "text-navy"
            }`}
          >
            {remaining} {remaining === 1 ? "day" : "days"} remaining
          </div>
        </section>
      ) : null}

      {/* Expired trial */}
      {snapshot !== null && trial !== null && !trial.active ? (
        <section
          aria-label="Expired free trial"
          data-testid="upgrade-cta"
          className="bg-white border border-border rounded-lg p-5 mb-4"
        >
          <h3 className="text-sm font-semibold text-text-muted uppercase tracking-wider mb-3">
            Free trial
          </h3>
          <p className="text-base font-semibold text-red mb-1">
            Your free trial has ended
          </p>
          <p className="text-[13px] text-text-muted mb-4">
            Ended on {trial.ends_at.slice(0, 10)}.
          </p>
          <Link
            to="/download"
            className="inline-flex items-center px-4 py-2 text-sm font-medium bg-red text-white rounded-md hover:bg-red-hover transition-colors no-underline"
          >
            Purchase a license
          </Link>
        </section>
      ) : null}

      {/* No trial started */}
      {snapshot !== null && trial === null ? (
        <section
          aria-label="Trial not started"
          className="bg-white border border-border rounded-lg p-5 mb-4"
        >
          <h3 className="text-sm font-semibold text-text-muted uppercase tracking-wider mb-3">
            Free trial
          </h3>
          <h3 className="text-base font-semibold text-navy mb-1">No free trial yet</h3>
          <p className="text-text-muted mb-0">
            Your 14-day free trial starts only when your first PC is
            connected.{" "}
            <Link to="/download" className="text-red hover:text-red-hover font-medium">
              Download RobloxBridge
            </Link>{" "}
            to get started.
          </p>
        </section>
      ) : null}

      {/* Paid license section */}
      {snapshot !== null ? (
        <section
          aria-label="Paid license"
          data-testid="license-state"
          className="bg-white border border-border rounded-lg p-5"
        >
          <h3 className="text-sm font-semibold text-text-muted uppercase tracking-wider mb-3">
            Paid license
          </h3>

          {license !== null ? (
            <dl className="grid grid-cols-[160px_1fr] gap-x-4 gap-y-3 text-sm">
              <dt className="text-text-muted font-medium">Status</dt>
              <dd className="text-navy font-semibold">
                {license.status} · {license.active_bindings}/{license.device_slots} slots used ·{" "}
                {license.available_slots} available
              </dd>

              <dt className="text-text-muted font-medium">Owner</dt>
              <dd data-testid="license-owner" className="text-navy">
                {snapshot.license.owner.display_name} ({snapshot.license.owner.roblox_id_masked})
              </dd>

              <dt className="text-text-muted font-medium">Expires</dt>
              <dd data-testid="license-expiry" className="text-navy">
                {license.expires_at !== null
                  ? license.expires_at.slice(0, 10)
                  : "Unavailable"}
              </dd>

              <dt className="text-text-muted font-medium">Subscription</dt>
              <dd data-testid="license-subscription" className="text-navy">
                {license.subscription_id !== null
                  ? license.subscription_id
                  : "Unavailable"}
              </dd>

              <dt className="text-text-muted font-medium">Slots</dt>
              <dd data-testid="license-slots" className="text-navy">
                {license.device_slots} total · {license.active_bindings} active ·{" "}
                {license.available_slots} available
              </dd>

              <dt className="text-text-muted font-medium">Scopes</dt>
              <dd data-testid="license-scopes" className="text-navy">
                {license.allowed_scopes.length > 0
                  ? license.allowed_scopes.join(" · ")
                  : "None allowed"}
              </dd>

              <dt className="text-text-muted font-medium">Usage</dt>
              <dd data-testid="license-usage" className="text-navy">
                {license.current_usage}{" "}
                {license.usage_limit !== null
                  ? `/ ${license.usage_limit.toLocaleString("en-US")}`
                  : "/ Unlimited"}
              </dd>

              <dt className="text-text-muted font-medium">Transfer</dt>
              <dd data-testid="license-transfer-status" className="text-navy">
                {license.transfer_status !== null
                  ? license.transfer_status.replace(/_/g, " ")
                  : "Unavailable"}
              </dd>

              <dt className="text-text-muted font-medium">Recovery</dt>
              <dd data-testid="license-recovery-status" className="text-navy">
                {license.recovery_status !== null
                  ? license.recovery_status.replace(/_/g, " ")
                  : "Unavailable"}
              </dd>
            </dl>
          ) : (
            <p className="text-text-muted mb-0">
              No paid license on this account.{" "}
              <Link to="/download" className="text-red hover:text-red-hover font-medium">
                Get a license
              </Link>{" "}
              to continue after the trial.
            </p>
          )}
        </section>
      ) : null}
    </section>
  );
}