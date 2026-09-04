import { useCallback, useState } from "react";
import { Navigate } from "react-router";
import {
  type AdminTrialPreview,
  ApiError,
  UnauthorizedError,
  adminExtendTrial,
  getAdminTrialPreview,
} from "../api/client";
import StatusBadge from "../components/StatusBadge";

// TrialExtension moves an existing trial entitlement's expiry later. The
// typed-confirmation form carries the entitlement id, the new UTC expiry, the
// case id, reason, evidence reference, and the expected version — the current
// expiry shown by the preview. The server updates the same row only; no
// second trial record can ever be created.
export default function TrialExtension() {
  const [userId, setUserId] = useState("");
  const [preview, setPreview] = useState<AdminTrialPreview | null>(null);
  const [denied, setDenied] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [notFound, setNotFound] = useState(false);
  const [failed, setFailed] = useState(false);
  const [entitlementId, setEntitlementId] = useState("");
  const [newEndsAt, setNewEndsAt] = useState("");
  const [caseId, setCaseId] = useState("");
  const [reason, setReason] = useState("");
  const [evidenceRef, setEvidenceRef] = useState("");
  const [expectedVersion, setExpectedVersion] = useState("");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(async () => {
    const id = userId.trim();
    if (id.length === 0) {
      return;
    }
    setBusy(true);
    setActionError(null);
    setNotice(null);
    setNotFound(false);
    setFailed(false);
    try {
      const loaded = await getAdminTrialPreview(id);
      setPreview(loaded);
      setExpectedVersion("");
    } catch (error) {
      setPreview(null);
      if (error instanceof UnauthorizedError) {
        setDenied(true);
      } else if (error instanceof ApiError && error.status === 403) {
        setForbidden(true);
      } else if (error instanceof ApiError && error.status === 404) {
        setNotFound(true);
      } else {
        setFailed(true);
      }
    } finally {
      setBusy(false);
    }
  }, [userId]);

  async function submit() {
    if (preview === null) {
      return;
    }
    setBusy(true);
    try {
      await adminExtendTrial({
        user_id: preview.user_id,
        entitlement_id: entitlementId.trim(),
        new_ends_at: newEndsAt.trim(),
        expected_version: expectedVersion.trim(),
        case_id: caseId.trim(),
        reason: reason.trim(),
        evidence_ref: evidenceRef.trim(),
      });
      setActionError(null);
      setNotice(
        "Extension completed. The same entitlement now ends at the new expiry; no second trial was created.",
      );
      setPreview(null);
      setEntitlementId("");
      setNewEndsAt("");
      setCaseId("");
      setReason("");
      setEvidenceRef("");
      setExpectedVersion("");
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        setActionError(
          "The request conflicts with the current trial state: the account changed since the preview, or the new expiry is not later than the current one.",
        );
      } else if (error instanceof ApiError && error.status === 404) {
        setActionError("No trial entitlement was found for this user.");
      } else if (error instanceof ApiError && error.status === 403) {
        setActionError("You do not have administrator access.");
      } else {
        setActionError("The extension failed. Please try again.");
      }
    } finally {
      setBusy(false);
    }
  }

  if (denied) {
    return <Navigate to="/login" replace />;
  }

  if (forbidden) {
    return (
      <section data-testid="page-admin-extension" aria-labelledby="extension-title">
        <h2 id="extension-title">Extend a trial</h2>
        <p data-testid="extension-forbidden" role="alert">
          You do not have administrator access.
        </p>
      </section>
    );
  }

  const versionMatches = preview !== null && expectedVersion.trim() === preview.version;
  const complete =
    preview !== null &&
    preview.trial !== null &&
    entitlementId.trim().length > 0 &&
    newEndsAt.trim().length > 0 &&
    caseId.trim().length > 0 &&
    reason.trim().length > 0 &&
    evidenceRef.trim().length > 0 &&
    versionMatches;

  return (
    <section data-testid="page-admin-extension" aria-labelledby="extension-title">
      <h2 id="extension-title">Extend a trial</h2>
      <p>
        Moves the expiry of an existing trial entitlement later. The same
        entitlement row is updated; no second trial record is created.
      </p>
      {actionError ? (
        <p role="alert" data-testid="extension-error">
          {actionError}
        </p>
      ) : null}
      {notice ? (
        <p role="status" data-testid="extension-success">
          {notice}
        </p>
      ) : null}
      {notFound ? <p role="alert">No readable account for this user id.</p> : null}
      {failed ? <p role="alert">The preview is unavailable right now. Try again.</p> : null}
      <form
        onSubmit={(event) => {
          event.preventDefault();
          if (preview === null) {
            void load();
          }
        }}
      >
        <label htmlFor="extension-user">Case user id</label>
        <input
          id="extension-user"
          data-testid="extension-user-id"
          value={userId}
          onChange={(event) => setUserId(event.target.value)}
        />
        <button
          type="submit"
          data-testid="extension-load"
          disabled={busy || userId.trim().length === 0}
        >
          Load preview
        </button>
      </form>
      {preview !== null ? (
        <>
          <div data-testid="extension-preview">
            <h3>Trial state</h3>
            {preview.trial !== null ? (
              <>
                <p>
                  Entitlement <code>{preview.trial.id}</code> —{" "}
                  <StatusBadge status={preview.trial.active ? "active" : "expired"} />
                </p>
                <p>
                  Started {preview.trial.started_at} · current expiry{" "}
                  <strong>{preview.trial.ends_at}</strong>
                </p>
              </>
            ) : (
              <p>No trial entitlement for this user.</p>
            )}
            <p>
              Expected version (the current expiry):{" "}
              <code data-testid="extension-version">{preview.version}</code>
            </p>
          </div>
          <p data-testid="extension-plan">
            The trial keeps its identity and start; only the expiry moves to
            the new UTC timestamp on the same entitlement id. No second trial
            record is created.
          </p>
          <form
            onSubmit={(event) => {
              event.preventDefault();
              if (complete && !busy) {
                void submit();
              }
            }}
          >
            <label htmlFor="extension-entitlement">Entitlement id</label>
            <input
              id="extension-entitlement"
              data-testid="extension-entitlement-id"
              value={entitlementId}
              onChange={(event) => setEntitlementId(event.target.value)}
            />
            <label htmlFor="extension-ends">New expiry (UTC, e.g. 2026-10-02T11:00:00Z)</label>
            <input
              id="extension-ends"
              data-testid="extension-new-ends-at"
              value={newEndsAt}
              onChange={(event) => setNewEndsAt(event.target.value)}
            />
            <label htmlFor="extension-case">Case id</label>
            <input
              id="extension-case"
              data-testid="extension-case-id"
              value={caseId}
              onChange={(event) => setCaseId(event.target.value)}
            />
            <label htmlFor="extension-reason">Reason</label>
            <input
              id="extension-reason"
              data-testid="extension-reason"
              value={reason}
              onChange={(event) => setReason(event.target.value)}
            />
            <label htmlFor="extension-evidence">Evidence reference</label>
            <input
              id="extension-evidence"
              data-testid="extension-evidence"
              value={evidenceRef}
              onChange={(event) => setEvidenceRef(event.target.value)}
            />
            <label htmlFor="extension-version-input">
              Expected version (type the current expiry shown above)
            </label>
            <input
              id="extension-version-input"
              data-testid="extension-expected-version"
              value={expectedVersion}
              onChange={(event) => setExpectedVersion(event.target.value)}
            />
            {!versionMatches && expectedVersion.trim().length > 0 ? (
              <p role="alert">The typed version does not match the current expiry.</p>
            ) : null}
            <button type="submit" data-testid="extension-submit" disabled={!complete || busy}>
              Confirm extension
            </button>
          </form>
        </>
      ) : null}
    </section>
  );
}
