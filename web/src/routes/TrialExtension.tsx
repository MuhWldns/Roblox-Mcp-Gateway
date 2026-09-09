import { useCallback, useState } from "react";
import { Navigate, useSearchParams } from "react-router";
import {
  type AdminTrialPreview,
  ApiError,
  UnauthorizedError,
  adminExtendTrial,
  getAdminTrialPreview,
} from "../api/client";
import StatusBadge from "../components/StatusBadge";

export default function TrialExtension() {
  const [params] = useSearchParams();
  const [userId, setUserId] = useState(() => params.get("user_id") ?? "");
  const [preview, setPreview] = useState<AdminTrialPreview | null>(null);
  const [denied, setDenied] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [notFound, setNotFound] = useState(false);
  const [failed, setFailed] = useState(false);
  const [newEndsAt, setNewEndsAt] = useState("");
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
      setNewEndsAt("");
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
    if (preview?.trial == null) {
      return;
    }
    setBusy(true);
    try {
      await adminExtendTrial({
        user_id: preview.user_id,
        entitlement_id: preview.trial.id,
        new_ends_at: newEndsAt.trim(),
        expected_version: preview.version,
        case_id: crypto.randomUUID(),
        reason: "Trial extension requested by administrator",
        evidence_ref: "admin-dashboard:extension",
      });
      setActionError(null);
      setNotice(
        "Extension completed. The same entitlement now ends at the new expiry; no second trial was created.",
      );
      setPreview(null);
      setNewEndsAt("");
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

  const complete = preview?.trial != null && preview.user_id === userId.trim() &&
    newEndsAt.trim().length > 0;

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
          void load();
        }}
      >
        <label htmlFor="extension-user">User id</label>
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
            <label htmlFor="extension-ends">New expiry (UTC, e.g. 2026-10-02T11:00:00Z)</label>
            <input
              id="extension-ends"
              data-testid="extension-new-ends-at"
              value={newEndsAt}
              onChange={(event) => setNewEndsAt(event.target.value)}
            />
            <button type="submit" data-testid="extension-submit" disabled={!complete || busy}>
              Confirm extension
            </button>
          </form>
        </>
      ) : null}
    </section>
  );
}
