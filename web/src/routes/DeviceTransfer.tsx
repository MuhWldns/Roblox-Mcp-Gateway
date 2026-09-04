import { useCallback, useState } from "react";
import { Navigate } from "react-router";
import {
  type AdminTransferPreview,
  ApiError,
  UnauthorizedError,
  adminTransferDevice,
  getAdminTransferPreview,
} from "../api/client";
import StatusBadge from "../components/StatusBadge";

// DeviceTransfer moves an active paid-license slot from one device to
// another. The typed-confirmation form carries the case id, reason, evidence
// reference, and the version token minted by the preview; the server rejects
// any request built on stale state.
export default function DeviceTransfer() {
  const [userId, setUserId] = useState("");
  const [preview, setPreview] = useState<AdminTransferPreview | null>(null);
  const [denied, setDenied] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [notFound, setNotFound] = useState(false);
  const [failed, setFailed] = useState(false);
  const [oldDeviceId, setOldDeviceId] = useState("");
  const [newDeviceId, setNewDeviceId] = useState("");
  const [licenseId, setLicenseId] = useState("");
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
      const loaded = await getAdminTransferPreview(id);
      setPreview(loaded);
      setOldDeviceId("");
      setNewDeviceId("");
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
      await adminTransferDevice({
        user_id: preview.user_id,
        license_id: licenseId.trim(),
        old_device_id: oldDeviceId,
        new_device_id: newDeviceId,
        expected_version: expectedVersion.trim(),
        case_id: caseId.trim(),
        reason: reason.trim(),
        evidence_ref: evidenceRef.trim(),
      });
      setActionError(null);
      setNotice("Transfer completed. The license slot moved to the new device.");
      setPreview(null);
      setCaseId("");
      setReason("");
      setEvidenceRef("");
      setExpectedVersion("");
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        setActionError(
          "The account changed since the preview was loaded. Reload the preview and try again.",
        );
      } else if (error instanceof ApiError && error.status === 404) {
        setActionError("The named license or device was not found for this user.");
      } else if (error instanceof ApiError && error.status === 403) {
        setActionError("You do not have administrator access.");
      } else {
        setActionError("The transfer failed. Please try again.");
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
      <section data-testid="page-admin-transfer" aria-labelledby="transfer-title">
        <h2 id="transfer-title">Transfer a license slot</h2>
        <p data-testid="transfer-forbidden" role="alert">
          You do not have administrator access.
        </p>
      </section>
    );
  }

  const activeDevices = preview?.devices.filter((device) => device.status === "active") ?? [];
  const versionMatches = preview !== null && expectedVersion.trim() === preview.version;
  const complete =
    preview !== null &&
    oldDeviceId.length > 0 &&
    newDeviceId.length > 0 &&
    licenseId.trim().length > 0 &&
    caseId.trim().length > 0 &&
    reason.trim().length > 0 &&
    evidenceRef.trim().length > 0 &&
    versionMatches &&
    oldDeviceId !== newDeviceId;

  return (
    <section data-testid="page-admin-transfer" aria-labelledby="transfer-title">
      <h2 id="transfer-title">Transfer a license slot</h2>
      <p>
        Moves an active paid-license slot from one device to another. The old
        device's live connection closes before the new binding activates.
      </p>
      {actionError ? (
        <p role="alert" data-testid="transfer-error">
          {actionError}
        </p>
      ) : null}
      {notice ? (
        <p role="status" data-testid="transfer-success">
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
        <label htmlFor="transfer-user">Case user id</label>
        <input
          id="transfer-user"
          data-testid="transfer-user-id"
          value={userId}
          onChange={(event) => setUserId(event.target.value)}
        />
        <button
          type="submit"
          data-testid="transfer-load"
          disabled={busy || userId.trim().length === 0}
        >
          Load preview
        </button>
      </form>
      {preview !== null ? (
        <>
          <div data-testid="transfer-preview">
            <h3>Account state</h3>
            <p>Acting on {preview.identity?.display_name ?? preview.user_id}</p>
            <ul>
              {preview.devices.map((device) => (
                <li key={device.id}>
                  {device.name} ({device.id}){" "}
                  <StatusBadge status={device.online ? "online" : "offline"} />{" "}
                  <StatusBadge status={device.status} />
                </li>
              ))}
            </ul>
            {preview.license !== null ? (
              <p>
                License status {preview.license.status} · device slots:{" "}
                {preview.license.device_slots} · active bindings:{" "}
                {preview.license.active_bindings}
              </p>
            ) : (
              <p>No active license for this user.</p>
            )}
            <p>
              State version token: <code data-testid="transfer-version">{preview.version}</code>
            </p>
          </div>
          <p data-testid="transfer-plan">
            {oldDeviceId.length > 0 && newDeviceId.length > 0
              ? `The license slot currently bound to ${oldDeviceId} moves to ${newDeviceId}. The live connection of ${oldDeviceId} closes before the new binding activates.`
              : "Pick the old and the new device to see what will happen."}
          </p>
          <form
            onSubmit={(event) => {
              event.preventDefault();
              if (complete && !busy) {
                void submit();
              }
            }}
          >
            <label htmlFor="transfer-license">License id</label>
            <input
              id="transfer-license"
              data-testid="transfer-license-id"
              value={licenseId}
              onChange={(event) => setLicenseId(event.target.value)}
            />
            <label htmlFor="transfer-old">Old device (currently bound)</label>
            <select
              id="transfer-old"
              data-testid="transfer-old-device"
              value={oldDeviceId}
              onChange={(event) => setOldDeviceId(event.target.value)}
            >
              <option value="">— pick a device —</option>
              {activeDevices.map((device) => (
                <option key={device.id} value={device.id}>
                  {device.name} ({device.id})
                </option>
              ))}
            </select>
            <label htmlFor="transfer-new">New device</label>
            <select
              id="transfer-new"
              data-testid="transfer-new-device"
              value={newDeviceId}
              onChange={(event) => setNewDeviceId(event.target.value)}
            >
              <option value="">— pick a device —</option>
              {activeDevices.map((device) => (
                <option key={device.id} value={device.id}>
                  {device.name} ({device.id})
                </option>
              ))}
            </select>
            <label htmlFor="transfer-case">Case id</label>
            <input
              id="transfer-case"
              data-testid="transfer-case-id"
              value={caseId}
              onChange={(event) => setCaseId(event.target.value)}
            />
            <label htmlFor="transfer-reason">Reason</label>
            <input
              id="transfer-reason"
              data-testid="transfer-reason"
              value={reason}
              onChange={(event) => setReason(event.target.value)}
            />
            <label htmlFor="transfer-evidence">Evidence reference</label>
            <input
              id="transfer-evidence"
              data-testid="transfer-evidence"
              value={evidenceRef}
              onChange={(event) => setEvidenceRef(event.target.value)}
            />
            <label htmlFor="transfer-version-input">
              Expected version (type the state version token shown above)
            </label>
            <input
              id="transfer-version-input"
              data-testid="transfer-expected-version"
              value={expectedVersion}
              onChange={(event) => setExpectedVersion(event.target.value)}
            />
            {!versionMatches && expectedVersion.trim().length > 0 ? (
              <p role="alert">The typed version does not match the previewed state.</p>
            ) : null}
            <button type="submit" data-testid="transfer-submit" disabled={!complete || busy}>
              Confirm transfer
            </button>
          </form>
        </>
      ) : null}
    </section>
  );
}
