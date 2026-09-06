import { useCallback, useEffect, useRef, useState } from "react";
import { Navigate, useSearchParams } from "react-router";
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
      setError("Enrollment code not found. Check the code shown in RobloxBridge.");
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
      setError("Approval failed. Please try again.");
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

  useEffect(() => {
    return () => {
      if (pollTimer.current !== null) {
        window.clearInterval(pollTimer.current);
      }
    };
  }, []);

  // Approval and first binding are distinct server events. Poll until Bridge
  // finishes the exchange and the server exposes the active trial.
  useEffect(() => {
    if (!approved || pollTimer.current !== null) return;

    let attempts = 0;
    pollTimer.current = window.setInterval(() => {
      attempts += 1;
      getMe()
        .then((profile) => {
          setMe(profile);
          if (profile.trial?.active && pollTimer.current !== null) {
            window.clearInterval(pollTimer.current);
            pollTimer.current = null;
          }
        })
        .catch(() => {
          // Transient reads do not invalidate an approval already accepted.
        });
      if (attempts >= pollLimit && pollTimer.current !== null) {
        window.clearInterval(pollTimer.current);
        pollTimer.current = null;
      }
    }, pollIntervalMs);
  }, [approved]);

  if (denied) {
    return <Navigate to="/login" replace />;
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
        Enroll a device
      </h2>
      <p className="text-text-secondary mb-2">Signed in as {me.display_name}</p>
      <p className="text-text-secondary mb-6">
        Review the device details before granting access. Your one-time 14-day
        trial starts only after RobloxBridge finishes connecting.
      </p>

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
              Enrollment code
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
                Show device
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
              <h3 className="text-base font-semibold text-navy mb-4">Confirm this device</h3>
              <dl className="grid grid-cols-[minmax(0,8rem)_minmax(0,1fr)] gap-x-4 gap-y-3 text-sm mb-5">
                <dt className="text-text-muted">Hostname</dt>
                <dd data-testid="device-hostname" className="text-navy font-semibold break-words">{claim.hostname}</dd>
                <dt className="text-text-muted">Platform</dt>
                <dd data-testid="device-platform" className="text-navy break-words">{claim.platform}</dd>
                <dt className="text-text-muted">Bridge version</dt>
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
                Approve device
              </button>
            </section>
          ) : null}
        </>
      ) : (
        <section
          data-testid="approval-status"
          aria-label="Enrollment approval result"
          className="bg-white border border-border rounded-lg p-6"
        >
          <h3 className="text-base font-semibold text-navy mb-2">Device approved</h3>
          <p className="text-text-secondary mb-4">
            RobloxBridge will connect automatically. Keep it running while this
            page waits for the first binding.
          </p>
          {me.trial?.active ? (
            <p data-testid="trial-state" className="font-semibold text-navy">
              Free trial active — ends {me.trial.ends_at.slice(0, 10)}
            </p>
          ) : (
            <p role="status" className="text-text-muted italic">
              Waiting for your device to finish enrollment…
            </p>
          )}
        </section>
      )}
    </section>
  );
}