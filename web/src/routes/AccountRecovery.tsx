import { useCallback, useState } from "react";
import { Navigate } from "react-router";
import {
  type AdminRecoveryPreview,
  ApiError,
  UnauthorizedError,
  adminRecoverIdentity,
  getAdminRecoveryPreview,
} from "../api/client";
import StatusBadge from "../components/StatusBadge";

// AccountRecovery revokes every web session, connector grant and token, and
// device credential of an account and drops its live Bridge connections. The
// typed-confirmation form carries the case id, reason, evidence reference,
// and the version token minted by the preview. The trial window is never
// touched — the plan says so explicitly.
export default function AccountRecovery() {
  const [userId, setUserId] = useState("");
  const [preview, setPreview] = useState<AdminRecoveryPreview | null>(null);
  const [denied, setDenied] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [notFound, setNotFound] = useState(false);
  const [failed, setFailed] = useState(false);
  const [newIdentityId, setNewIdentityId] = useState("");
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
      const loaded = await getAdminRecoveryPreview(id);
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
      await adminRecoverIdentity({
        user_id: preview.user_id,
        expected_version: expectedVersion.trim(),
        case_id: caseId.trim(),
        reason: reason.trim(),
        evidence_ref: evidenceRef.trim(),
        new_identity_id: newIdentityId.trim() === "" ? undefined : newIdentityId.trim(),
      });
      setActionError(null);
      setNotice(
        "Recovery completed. Every session, connector grant and token, and device credential was revoked; live connections were dropped. The trial window is unchanged.",
      );
      setPreview(null);
      setCaseId("");
      setReason("");
      setEvidenceRef("");
      setExpectedVersion("");
      setNewIdentityId("");
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        setActionError(
          "The account changed since the preview was loaded. Reload the preview and try again.",
        );
      } else if (error instanceof ApiError && error.status === 404) {
        setActionError("The named user was not found.");
      } else if (error instanceof ApiError && error.status === 403) {
        setActionError("You do not have administrator access.");
      } else {
        setActionError("The recovery failed. Please try again.");
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
      <section data-testid="page-admin-recovery" aria-labelledby="recovery-title">
        <h2 id="recovery-title">Run an identity recovery</h2>
        <p data-testid="recovery-forbidden" role="alert">
          You do not have administrator access.
        </p>
      </section>
    );
  }

  const versionMatches = preview !== null && expectedVersion.trim() === preview.version;
  const complete =
    preview !== null &&
    caseId.trim().length > 0 &&
    reason.trim().length > 0 &&
    evidenceRef.trim().length > 0 &&
    versionMatches;

  return (
    <section data-testid="page-admin-recovery" aria-labelledby="recovery-title">
      <h2 id="recovery-title">Run an identity recovery</h2>
      <p>
        Revokes every web session, connector grant and token, and device
        credential of the account and disconnects its live Bridge connections.
      </p>
      {actionError ? (
        <p role="alert" data-testid="recovery-error">
          {actionError}
        </p>
      ) : null}
      {notice ? (
        <p role="status" data-testid="recovery-success">
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
        <label htmlFor="recovery-user">Case user id</label>
        <input
          id="recovery-user"
          data-testid="recovery-user-id"
          value={userId}
          onChange={(event) => setUserId(event.target.value)}
        />
        <button
          type="submit"
          data-testid="recovery-load"
          disabled={busy || userId.trim().length === 0}
        >
          Load preview
        </button>
      </form>
      {preview !== null ? (
        <>
          <div data-testid="recovery-preview">
            <h3>Account state</h3>
            <p>Acting on {preview.identity?.display_name ?? preview.user_id}</p>
            <p>{preview.devices.length} device(s) registered</p>
            <ul>
              {preview.devices.map((device) => (
                <li key={device.id}>
                  {device.name} ({device.id}){" "}
                  <StatusBadge status={device.online ? "online" : "offline"} />{" "}
                  <StatusBadge status={device.status} />
                </li>
              ))}
            </ul>
            <p>{preview.connectors.length} connector grant(s)</p>
            <ul>
              {preview.connectors.map((connector) => (
                <li key={connector.id}>
                  {connector.client_name} ({connector.id}){" "}
                  <StatusBadge status={connector.revoked_at ? "revoked" : "active"} />
                </li>
              ))}
            </ul>
            <p>
              State version token: <code data-testid="recovery-version">{preview.version}</code>
            </p>
          </div>
          <p data-testid="recovery-plan">
            Every web session, connector grant and its access and refresh
            tokens, and every device credential will be revoked immediately;
            any live Bridge connection is dropped. The trial window is not
            changed.
          </p>
          <form
            onSubmit={(event) => {
              event.preventDefault();
              if (complete && !busy) {
                void submit();
              }
            }}
          >
            <label htmlFor="recovery-new-identity">New identity id (optional)</label>
            <input
              id="recovery-new-identity"
              data-testid="recovery-new-identity"
              value={newIdentityId}
              onChange={(event) => setNewIdentityId(event.target.value)}
            />
            <label htmlFor="recovery-case">Case id</label>
            <input
              id="recovery-case"
              data-testid="recovery-case-id"
              value={caseId}
              onChange={(event) => setCaseId(event.target.value)}
            />
            <label htmlFor="recovery-reason">Reason</label>
            <input
              id="recovery-reason"
              data-testid="recovery-reason"
              value={reason}
              onChange={(event) => setReason(event.target.value)}
            />
            <label htmlFor="recovery-evidence">Evidence reference</label>
            <input
              id="recovery-evidence"
              data-testid="recovery-evidence"
              value={evidenceRef}
              onChange={(event) => setEvidenceRef(event.target.value)}
            />
            <label htmlFor="recovery-version-input">
              Expected version (type the state version token shown above)
            </label>
            <input
              id="recovery-version-input"
              data-testid="recovery-expected-version"
              value={expectedVersion}
              onChange={(event) => setExpectedVersion(event.target.value)}
            />
            {!versionMatches && expectedVersion.trim().length > 0 ? (
              <p role="alert">The typed version does not match the previewed state.</p>
            ) : null}
            <button type="submit" data-testid="recovery-submit" disabled={!complete || busy}>
              Confirm recovery
            </button>
          </form>
        </>
      ) : null}
    </section>
  );
}
