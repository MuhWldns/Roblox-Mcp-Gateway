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

export default function Enroll() {
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

  // Opening the verification URL with an embedded code reviews the device
  // immediately; typing a code manually uses the form.
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
      .catch((err: unknown) => {
        if (err instanceof UnauthorizedError && !cancelled) {
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

  // Once approved, poll until the device completes its exchange and the
  // first-device trial starts.
  useEffect(() => {
    if (!approved || pollTimer.current !== null) {
      return;
    }
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
          /* keep polling until the device finishes its exchange */
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
  if (!me) {
    return (
      <main>
        <p role="status">Loading…</p>
      </main>
    );
  }

  return (
    <main>
      <h1>Enroll a device</h1>
      <p>Signed in as {me.display_name}</p>

      {!approved ? (
        <>
          <form
            onSubmit={(event) => {
              event.preventDefault();
              void loadClaim(code);
            }}
          >
            <label htmlFor="enrollment-code">Enrollment code</label>
            <input
              id="enrollment-code"
              name="code"
              value={code}
              onChange={(event) => setCode(event.target.value)}
              placeholder="rkuc_…"
            />
            <button type="submit" disabled={busy || code.trim().length === 0}>
              Show device
            </button>
          </form>
          {error ? <p role="alert">{error}</p> : null}

          {claim ? (
            <section aria-label="Device requesting enrollment">
              <h2>Confirm this device</h2>
              <p>
                Device <strong data-testid="device-hostname">{claim.hostname}</strong> on
                platform {claim.platform} running RobloxBridge {claim.bridge_version} is
                asking to join your account.
              </p>
              <button type="button" onClick={() => void approve()} disabled={busy}>
                Approve device
              </button>
              <p>
                Approving this device starts your one-time 14-day free trial only
                after RobloxBridge finishes connecting.
              </p>
            </section>
          ) : null}
        </>
      ) : (
        <section aria-label="Enrollment approval result">
          <p data-testid="approval-status">
            Device approved. RobloxBridge will connect automatically — keep it
            running. This page updates when your trial starts.
          </p>
          {me.trial?.active ? (
            <p data-testid="trial-state">
              Free trial active — ends {me.trial.ends_at.slice(0, 10)}
            </p>
          ) : (
            <p role="status">Waiting for your device to finish enrollment…</p>
          )}
        </section>
      )}
    </main>
  );
}
