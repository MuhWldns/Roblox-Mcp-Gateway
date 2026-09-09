import { useCallback, useEffect, useRef, useState } from "react";
import { Link, Navigate, useSearchParams } from "react-router";
import {
  type EnrollmentClaim,
  type MeResponse,
  UnauthorizedError,
  approveEnrollment,
  getEnrollmentClaim,
  getMe,
} from "../api/client";

const pollIntervalMs = 500;
const pollLimit = 120;

// EnrollPage handles the device enrollment flow: Bridge generates a code,
// the user reviews the requesting device and explicitly approves it.
export default function EnrollPage() {
  const [searchParams] = useSearchParams();
  const urlCode = searchParams.get("code") ?? "";
  const [code, setCode] = useState(urlCode);
  const [claim, setClaim] = useState<EnrollmentClaim | null>(null);
  const [approved, setApproved] = useState(false);
  const [me, setMe] = useState<MeResponse | null>(null);
  const [denied, setDenied] = useState(false);
  const [licenseRequired, setLicenseRequired] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const pollTimer = useRef<number | null>(null);
  const loadClaim = useCallback(async (value: string) => {
    setBusy(true);
    setError(null);
    try {
      const pending = await getEnrollmentClaim(value);
      setClaim(pending);
    } catch {
      setError("We couldn’t load this pairing code. Check the code in Buildly Companion and try again.");
    } finally {
      setBusy(false);
    }
  }, []);

  const approve = useCallback(async () => {
    setBusy(true);
    setError(null);
    try {
      await approveEnrollment(code);
      setApproved(true);
    } catch {
      setError("We couldn’t approve this computer. Check your connection and try again.");
    } finally {
      setBusy(false);
    }
  }, [code]);

  useEffect(() => {
    if (urlCode !== "") {
      void loadClaim(urlCode);
    }
  }, [urlCode, loadClaim]);

  useEffect(() => {
    let cancelled = false;
    getMe()
      .then((profile) => {
        if (!cancelled) setMe(profile);
      })
      .catch((failure: unknown) => {
        if (failure instanceof UnauthorizedError && !cancelled) {
          setDenied(true);
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Approval and first binding are distinct server events. Poll until Bridge
  // finishes the exchange and the server exposes the active trial or reports
  // that a license is required.
  useEffect(() => {
    if (!approved || licenseRequired) return;

    let cancelled = false;
    let attempts = 0;
    const pollCode = code.trim();

    const tick = async () => {
      attempts += 1;

      try {
        const claimPromise =
          pollCode !== ""
            ? getEnrollmentClaim(pollCode).catch(() => null)
            : Promise.resolve(null);
        const mePromise = getMe().catch(() => null);

        const [claimResult, meResult] = await Promise.all([
          claimPromise,
          mePromise,
        ]);

        if (cancelled) return;

        if (claimResult && claimResult.status === "license_required") {
          setLicenseRequired(true);
          if (pollTimer.current !== null) {
            window.clearTimeout(pollTimer.current);
            pollTimer.current = null;
          }
          return;
        }

        if (meResult) {
          setMe(meResult);
          if (meResult.trial?.active) {
            if (pollTimer.current !== null) {
              window.clearTimeout(pollTimer.current);
              pollTimer.current = null;
            }
            return;
          }
        }
      } catch {
        // Transient reads do not invalidate an approval already accepted.
      }

      if (cancelled) return;

      if (attempts >= pollLimit) {
        if (pollTimer.current !== null) {
          window.clearTimeout(pollTimer.current);
          pollTimer.current = null;
        }
        return;
      }

      pollTimer.current = window.setTimeout(() => {
        void tick();
      }, pollIntervalMs);
    };

    pollTimer.current = window.setTimeout(() => {
      void tick();
    }, pollIntervalMs);

    return () => {
      cancelled = true;
      if (pollTimer.current !== null) {
        window.clearTimeout(pollTimer.current);
        pollTimer.current = null;
      }
    };
  }, [approved, licenseRequired, code]);

  if (denied) {
    return <Navigate to={`/login?${new URLSearchParams({ next: `/enroll?${new URLSearchParams({ code })}` })}`} replace />;
  }
  if (me === null) {
    return <p role="status" className="text-text-muted italic">Loading account…</p>;
  }

  return (
    <section
      data-testid="page-enroll"
      aria-labelledby="enroll-title"
      className="animate-[pageEnter_200ms_ease]"
    >
      <h2 id="enroll-title" className="text-xl font-semibold text-navy mb-1">
        Connect this computer?
      </h2>
      <p className="text-text-secondary mb-2">Signed in as {me.display_name}</p>
      <p className="text-text-secondary mb-6">
        Check that the computer below is yours. Only approve it if you started this
        setup. Your 14-day trial starts when your first computer finishes connecting.
      </p>
      <Link to="/setup" className="inline-flex min-h-11 items-center mb-4 underline">Back to setup</Link>

      {!approved ? (
        <>
          <form
            className="bg-white border border-border rounded-lg p-6 mb-5"
            onSubmit={(event) => {
              event.preventDefault();
              void loadClaim(code.trim());
            }}
          >
            <label htmlFor="enrollment-code" className="block text-sm font-semibold text-navy mb-1">
              Pairing code
            </label>
            <div className="flex flex-col sm:flex-row gap-3">
              <input
                id="enrollment-code"
                name="code"
                value={code}
                onChange={(event) => setCode(event.target.value)}
                placeholder="rkuc_…"
                autoComplete="off"
                className="min-w-0 flex-1 text-base text-navy bg-white border border-border rounded-md px-3 py-2 focus:outline-none focus:border-red focus:shadow-[0_0_0_3px_var(--color-red-light)]"
              />
              <button
                type="submit"
                disabled={busy || code.trim().length === 0}
                className="px-4 py-2 text-sm font-medium bg-navy text-white rounded-md hover:bg-navy-light disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
              >
                Show computer
              </button>
            </div>
          </form>

          {error ? (
            <p role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium mb-4">
              {error}
            </p>
          ) : null}

          {claim !== null ? (
            <section aria-label="Device requesting enrollment" className="bg-white border border-border rounded-lg p-6">
              <h3 className="text-base font-semibold text-navy mb-4">Review this computer</h3>
              <dl className="grid grid-cols-[minmax(0,8rem)_minmax(0,1fr)] gap-x-4 gap-y-3 text-sm mb-5">
                <dt className="text-text-muted">Computer name</dt>
                <dd data-testid="device-hostname" className="text-navy font-semibold break-words">{claim.hostname}</dd>
                <dt className="text-text-muted">Platform</dt>
                <dd data-testid="device-platform" className="text-navy break-words">{claim.platform}</dd>
                <dt className="text-text-muted">Companion version</dt>
                <dd data-testid="device-bridge-version" className="text-navy break-words">{claim.bridge_version}</dd>
                <dt className="text-text-muted">Request expires</dt>
                <dd data-testid="device-started-at" className="text-navy break-words">
                  {claim.expires_at.slice(0, 19).replace("T", " ")} UTC
                </dd>
              </dl>
              <button
                type="button"
                onClick={() => void approve()}
                disabled={busy}
                className="px-4 py-2 text-sm font-medium bg-red text-white rounded-md hover:bg-red-hover disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
              >
                Approve computer
              </button>
            </section>
          ) : null}
        </>
      ) : licenseRequired ? (
        <section
          data-testid="license-required-status"
          aria-label="License required"
          className="bg-white border border-border rounded-lg p-6"
        >
          <p role="alert" className="text-navy font-semibold mb-4">
            You don’t have a license. Please contact support to get a license.
          </p>
          <Link to="/setup" className="inline-flex min-h-11 items-center underline">
            Back to setup
          </Link>
        </section>
      ) : (
        <section
          data-testid="approval-status"
          aria-label="Enrollment approval result"
          className="bg-white border border-border rounded-lg p-6"
        >
          <h3 className="text-base font-semibold text-navy mb-2">Computer approved</h3>
          <p className="text-text-secondary mb-4">
            Keep Buildly Companion running on your computer to finish connecting.
          </p>
          {me.trial?.active ? (
            <p data-testid="trial-state" className="font-semibold text-navy">
              Free trial active — ends {me.trial.ends_at.slice(0, 10)}
            </p>
          ) : (
            <p role="status" className="text-text-muted italic">
              Waiting for your computer to finish connecting…
            </p>
          )}
          <Link to="/setup" className="inline-flex mt-5 min-h-11 items-center rounded-md bg-red text-white px-5 font-semibold">Continue setup</Link>
        </section>
      )}
    </section>
  );
}