import { useCallback, useEffect, useState } from "react";
import { Link, Navigate } from "react-router";
import {
  type DeviceView,
  UnauthorizedError,
  getDevices,
  renameDevice,
  revokeDevice,
} from "../api/client";
import ConfirmDialog from "../components/ConfirmDialog";
import StatusBadge from "../components/StatusBadge";

// Devices lists the account's Bridges with live presence, the device
// lifecycle state, and the two self-service mutations: rename and revoke.
// Revocation is deliberate and slot-keeping — the warning spells that out.
export default function Devices() {
  const [devices, setDevices] = useState<DeviceView[] | null>(null);
  const [denied, setDenied] = useState(false);
  const [failed, setFailed] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<DeviceView | null>(null);
  const [renaming, setRenaming] = useState<DeviceView | null>(null);
  const [draftName, setDraftName] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const list = await getDevices();
      setDevices(list.devices);
      setFailed(false);
    } catch (error) {
      if (error instanceof UnauthorizedError) {
        setDenied(true);
        return;
      }
      setFailed(true);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function confirmRevoke() {
    if (revoking === null) {
      return;
    }
    setBusy(true);
    try {
      await revokeDevice(revoking.id);
      setRevoking(null);
      setActionError(null);
      setNotice("Device revoked. Its license slot stays used.");
      await load();
    } catch (error) {
      setRevoking(null);
      if (error instanceof UnauthorizedError) {
        setDenied(true);
      } else {
        setActionError("Revoking the device failed. Please try again.");
      }
    } finally {
      setBusy(false);
    }
  }

  async function saveRename() {
    if (renaming === null) {
      return;
    }
    const name = draftName.trim();
    if (name.length === 0) {
      return;
    }
    setBusy(true);
    try {
      await renameDevice(renaming.id, name);
      setRenaming(null);
      setActionError(null);
      setNotice("Device renamed.");
      await load();
    } catch (error) {
      setRenaming(null);
      if (error instanceof UnauthorizedError) {
        setDenied(true);
      } else {
        setActionError("Renaming the device failed. Please try again.");
      }
    } finally {
      setBusy(false);
    }
  }

  if (denied) {
    return <Navigate to="/login" replace />;
  }

  return (
    <section data-testid="page-devices" aria-labelledby="devices-title">
      <h2 id="devices-title">Devices</h2>
      <p>
        Bridges enrolled on your account. Revoking a device does not free its
        license slot.
      </p>
      {actionError ? <p role="alert">{actionError}</p> : null}
      {notice ? <p role="status">{notice}</p> : null}
      {failed ? <p role="alert">Devices unavailable right now. Reload to try again.</p> : null}
      {devices === null && !failed ? <p role="status">Loading devices…</p> : null}
      {devices !== null && devices.length === 0 ? (
        <p>
          No devices yet. <Link to="/download">Download RobloxBridge</Link> and
          enroll your first device — enrollment is what starts your free trial.
        </p>
      ) : null}
      {devices !== null && devices.length > 0 ? (
        <ul>
          {devices.map((device) => (
            <li key={device.id} data-testid={`device-${device.id}`}>
              <h3>{device.name}</h3>
              <p>
                <StatusBadge status={device.online ? "online" : "offline"} />
              </p>
              <p>
                <StatusBadge status={device.status} />
              </p>
              <p>
                Enrolled {device.created_at.slice(0, 10)} · last updated{" "}
                {device.updated_at.slice(0, 10)}
              </p>
              {renaming !== null && renaming.id === device.id ? (
                <form
                  onSubmit={(event) => {
                    event.preventDefault();
                    void saveRename();
                  }}
                >
                  <label htmlFor={`rename-${device.id}`}>Device name</label>
                  <input
                    id={`rename-${device.id}`}
                    name="name"
                    value={draftName}
                    onChange={(event) => setDraftName(event.target.value)}
                  />
                  <button type="submit" disabled={busy || draftName.trim().length === 0}>
                    Save name
                  </button>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => {
                      setRenaming(null);
                      setDraftName("");
                    }}
                  >
                    Cancel rename
                  </button>
                </form>
              ) : (
                <button
                  type="button"
                  onClick={() => {
                    setRenaming(device);
                    setDraftName(device.name);
                  }}
                >
                  Rename
                </button>
              )}
              {device.status !== "revoked" ? (
                <button type="button" onClick={() => setRevoking(device)}>
                  Revoke device
                </button>
              ) : null}
            </li>
          ))}
        </ul>
      ) : null}
      {revoking !== null ? (
        <ConfirmDialog
          title="Revoke this device?"
          body={
            <>
              <p>
                Revoking <strong>{revoking.name}</strong> disconnects RobloxBridge
                immediately and permanently disables this device's credential.
              </p>
              <p data-testid="revoke-slot-warning">
                This does not free a license slot: the slot this device occupies
                stays used.
              </p>
            </>
          }
          confirmLabel="Yes, revoke this device"
          busy={busy}
          onConfirm={() => void confirmRevoke()}
          onCancel={() => setRevoking(null)}
        />
      ) : null}
    </section>
  );
}
