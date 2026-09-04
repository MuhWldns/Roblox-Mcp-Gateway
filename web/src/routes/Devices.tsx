import { useCallback, useEffect, useState } from "react";
import { Link, Navigate } from "react-router";
import {
  type DeviceView,
  type RotatedDeviceCredential,
  UnauthorizedError,
  getDevices,
  renameDevice,
  revokeDevice,
  rotateDeviceCredential,
} from "../api/client";
import ConfirmDialog from "../components/ConfirmDialog";
import StatusBadge from "../components/StatusBadge";

// Devices lists enrolled Bridges with their current operational state and
// owner actions. A rotated credential exists only in component memory until
// it is dismissed or this route unmounts.
export default function Devices() {
  const [devices, setDevices] = useState<DeviceView[] | null>(null);
  const [denied, setDenied] = useState(false);
  const [failed, setFailed] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<DeviceView | null>(null);
  const [renaming, setRenaming] = useState<DeviceView | null>(null);
  const [rotating, setRotating] = useState<DeviceView | null>(null);
  const [rotatedCredential, setRotatedCredential] =
    useState<RotatedDeviceCredential | null>(null);
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

  async function confirmRotation() {
    if (rotating === null) {
      return;
    }
    setBusy(true);
    setRotatedCredential(null);
    try {
      const rotated = await rotateDeviceCredential(rotating.id);
      setRotating(null);
      setActionError(null);
      setNotice(null);
      setRotatedCredential(rotated);
      await load();
    } catch (error) {
      setRotating(null);
      if (error instanceof UnauthorizedError) {
        setDenied(true);
      } else {
        setActionError(
          "Rotating the credential failed. The existing credential is still active. Please try again.",
        );
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
      {rotatedCredential ? (
        <section aria-labelledby="rotated-credential-title" role="status">
          <h3 id="rotated-credential-title">New device credential</h3>
          <p id="credential-once-warning" data-testid="credential-once-warning">
            Copy and store this credential securely now. It will not be shown again
            after you leave, refresh, or dismiss this message.
          </p>
          <p>
            <code
              data-testid="rotated-credential"
              aria-describedby="credential-once-warning"
            >
              {rotatedCredential.device_credential}
            </code>
          </p>
          <button type="button" onClick={() => setRotatedCredential(null)}>
            I stored it
          </button>
        </section>
      ) : null}
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
              <dl>
                <dt>Hostname</dt>
                <dd data-testid="device-hostname">{device.hostname ?? "Unavailable"}</dd>
                <dt>Platform</dt>
                <dd data-testid="device-platform">{device.platform ?? "Unavailable"}</dd>
                <dt>Bridge version</dt>
                <dd data-testid="device-bridge-version">
                  {device.bridge_version ?? "Unavailable"}
                </dd>
                <dt>Last heartbeat</dt>
                <dd data-testid="device-last-heartbeat">
                  {device.last_heartbeat_at?.slice(0, 19).replace("T", " ") ?? "Unavailable"}
                </dd>
                <dt>Official MCP state</dt>
                <dd data-testid="device-mcp-state">
                  {device.official_mcp_state ? (
                    <StatusBadge status={device.official_mcp_state} />
                  ) : (
                    "Unavailable"
                  )}
                </dd>
                <dt>Reconnect count</dt>
                <dd data-testid="device-reconnect-count">{device.reconnect_count}</dd>
                <dt>Last error</dt>
                <dd data-testid="device-last-error">{device.last_error ?? "None reported"}</dd>
                <dt>Enrollment</dt>
                <dd>
                  Enrolled {device.created_at.slice(0, 10)} · last updated{" "}
                  {device.updated_at.slice(0, 10)}
                </dd>
              </dl>
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
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => {
                    setRotatedCredential(null);
                    setRotating(device);
                  }}
                >
                  Rotate credential
                </button>
              ) : null}
              {device.status !== "revoked" ? (
                <button type="button" onClick={() => setRevoking(device)}>
                  Revoke device
                </button>
              ) : null}
            </li>
          ))}
        </ul>
      ) : null}
      {rotating !== null ? (
        <ConfirmDialog
          title="Rotate this credential?"
          body={
            <>
              <p>
                Rotating <strong>{rotating.name}</strong>'s credential immediately
                invalidates the current credential and disconnects its Bridge.
              </p>
              <p>
                The replacement is shown only once. Be ready to copy and store it
                securely before continuing.
              </p>
            </>
          }
          confirmLabel="Yes, rotate credential"
          busy={busy}
          onConfirm={() => void confirmRotation()}
          onCancel={() => setRotating(null)}
        />
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
