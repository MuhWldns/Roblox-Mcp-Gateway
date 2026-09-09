import { useCallback, useState } from "react";
import { Navigate, useSearchParams } from "react-router";
import {
  type AdminRecoveryPreview,
  ApiError,
  UnauthorizedError,
  adminDisconnectDevice,
  adminRecoverIdentity,
  adminRestoreDevice,
  getAdminRecoveryPreview,
} from "../api/client";
import StatusBadge from "../components/StatusBadge";

export default function AccountRecovery() {
  const [params] = useSearchParams();
  const [userId, setUserId] = useState(() => params.get("user_id") ?? "");
  const [preview, setPreview] = useState<AdminRecoveryPreview | null>(null);
  const [denied, setDenied] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [notFound, setNotFound] = useState(false);
  const [failed, setFailed] = useState(false);
  const [newIdentityId, setNewIdentityId] = useState("");
  const [restoringId, setRestoringId] = useState<string | null>(null);
  const [disconnectingId, setDisconnectingId] = useState<string | null>(null);
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
      setActionError(null);
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
        expected_version: preview.version,
        case_id: crypto.randomUUID(),
        reason: "Account recovery requested by administrator",
        evidence_ref: "admin-dashboard:recovery",
        new_identity_id: newIdentityId.trim() === "" ? undefined : newIdentityId.trim(),
      });
      setActionError(null);
      setNotice(
        "Recovery completed. Every session, connector grant and token, and device credential was revoked; live connections were dropped. The trial window is unchanged.",
      );
      setPreview(null);
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

  async function handleRestoreDevice(deviceId: string) {
    if (!preview) return;
    setRestoringId(deviceId);
    setActionError(null);
    try {
      await adminRestoreDevice(preview.user_id, deviceId);
      setNotice("Device un-revoked successfully. The operator can now re-enroll or pair it with a fresh code.");
      const reloaded = await getAdminRecoveryPreview(userId.trim());
      setPreview(reloaded);
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "Failed to restore device.");
    } finally {
      setRestoringId(null);
    }
  }

  async function handleDisconnectDevice(deviceId: string) {
    if (!preview) return;
    setDisconnectingId(deviceId);
    setActionError(null);
    try {
      await adminDisconnectDevice(preview.user_id, deviceId);
      setNotice("Device WebSocket connection disconnected. The Bridge client will reconnect on its next poll/launch.");
      const reloaded = await getAdminRecoveryPreview(userId.trim());
      setPreview(reloaded);
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "Failed to disconnect device.");
    } finally {
      setDisconnectingId(null);
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

  const complete = preview !== null && preview.user_id === userId.trim();

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
          void load();
        }}
      >
        <label htmlFor="recovery-user">User id</label>
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
            <ul className="list-none p-0 m-0 space-y-2 my-3">
                {preview.devices.map((device) => (
                  <li
                    key={device.id}
                    className="p-3 bg-surface-alt rounded border border-border flex flex-wrap items-center justify-between gap-3 text-sm"
                  >
                    <div>
                      <span className="font-semibold text-navy mr-2">{device.name}</span>
                      <span className="text-text-muted">({device.id})</span>
                      <div className="mt-1 flex items-center gap-2">
                        <StatusBadge status={device.online ? "online" : "offline"} />
                        <StatusBadge status={device.status} />
                      </div>
                    </div>
                    <div className="flex gap-2">
                      {device.online ? (
                        <button
                          type="button"
                          disabled={busy || disconnectingId === device.id}
                          onClick={() => void handleDisconnectDevice(device.id)}
                          className="px-3 py-1.5 text-xs font-semibold border border-border rounded bg-white text-navy hover:bg-surface-alt transition-colors"
                        >
                          {disconnectingId === device.id ? "Disconnecting…" : "Disconnect"}
                        </button>
                      ) : null}
                      {device.status === "revoked" ? (
                        <button
                          type="button"
                          disabled={busy || restoringId === device.id}
                          onClick={() => void handleRestoreDevice(device.id)}
                          className="px-3 py-1.5 text-xs font-semibold border border-red text-red rounded bg-white hover:bg-error-bg transition-colors"
                        >
                          {restoringId === device.id ? "Restoring…" : "Un-revoke / Restore"}
                        </button>
                      ) : null}
                    </div>
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
              void submit();
            }}
          >
            <label htmlFor="recovery-identity">
              Replacement Roblox identity id (optional)
            </label>
            <input
              id="recovery-identity"
              data-testid="recovery-new-identity"
              value={newIdentityId}
              onChange={(event) => setNewIdentityId(event.target.value)}
            />
            <button type="submit" data-testid="recovery-submit" disabled={!complete || busy}>
              Confirm recovery
            </button>
          </form>
        </>
      ) : null}
    </section>
  );
}
