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
    } catch (error: unknown) {
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
    if (revoking === null) return;
    setBusy(true);
    setActionError(null);
    try {
      await revokeDevice(revoking.id);
      setRevoking(null);
      setNotice("Device revoked.");
      await load();
    } catch (error: unknown) {
      setActionError(
        error instanceof Error ? error.message : "Revoke failed. Try again.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function confirmRotation() {
    if (rotating === null) return;
    setBusy(true);
    setActionError(null);
    try {
      const credential = await rotateDeviceCredential(rotating.id);
      setRotating(null);
      setRotatedCredential(credential);
      await load();
    } catch {
      setActionError(
        "Rotating the credential failed. The existing credential is still active; try again.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function saveRename() {
    if (renaming === null) return;
    const name = draftName.trim();
    if (name.length === 0) return;
    setBusy(true);
    setActionError(null);
    try {
      await renameDevice(renaming.id, name);
      setRenaming(null);
      setDraftName("");
      setNotice("Device renamed.");
      await load();
    } catch (error: unknown) {
      setActionError(
        error instanceof Error ? error.message : "Rename failed. Try again.",
      );
    } finally {
      setBusy(false);
    }
  }

  if (denied) {
    return <Navigate to="/login" replace />;
  }

  return (
    <section
      data-testid="page-devices"
      aria-labelledby="devices-title"
      className="animate-[pageEnter_200ms_ease]"
    >
      <h2 id="devices-title" className="text-xl font-semibold text-navy mb-1">
        Devices
      </h2>
      <p className="text-text-secondary mb-6">
        Bridges enrolled on your account. Revoking a device does not free its
        license slot.
      </p>
      {actionError ? (
        <div role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium mb-4">
          {actionError}
        </div>
      ) : null}
      {notice ? (
        <div role="status" className="bg-info-bg text-info border border-info rounded-md px-4 py-3 text-sm mb-4">
          {notice}
        </div>
      ) : null}
      {rotatedCredential ? (
        <section
          aria-labelledby="rotated-credential-title"
          role="status"
          className="bg-warning-bg border border-warning rounded-lg p-5 mb-5"
        >
          <h3 id="rotated-credential-title" className="text-warning mb-2 font-semibold">
            New device credential
          </h3>
          <p id="credential-once-warning" data-testid="credential-once-warning" className="text-sm text-text-secondary mb-3">
            Copy and store this credential securely now. It will not be shown again
            after you leave, refresh, or dismiss this message.
          </p>
          <code
            data-testid="rotated-credential"
            aria-describedby="credential-once-warning"
            className="block bg-white border border-border rounded-md p-3 text-[13px] break-all mb-3 font-mono"
          >
            {rotatedCredential.device_credential}
          </code>
          <button
            type="button"
            onClick={() => setRotatedCredential(null)}
            className="px-4 py-2 text-sm font-medium bg-red text-white border border-red rounded-md hover:bg-red-hover transition-colors"
          >
            I stored it
          </button>
        </section>
      ) : null}
      {failed ? (
        <div role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium mb-4">
          Devices unavailable right now. Reload to try again.
        </div>
      ) : null}
      {devices === null && !failed ? (
        <p role="status" className="text-text-muted italic">Loading devices…</p>
      ) : null}
      {devices !== null && devices.length === 0 ? (
        <div className="text-center py-12 px-6 bg-white border-2 border-dashed border-border rounded-lg">
          <p className="text-text-muted mb-4">
            No devices yet.{" "}
            <Link to="/download" className="text-red hover:text-red-hover font-medium">
              Download RobloxBridge
            </Link>{" "}
            and enroll your first device — enrollment is what starts your free trial.
          </p>
        </div>
      ) : null}
      {devices !== null && devices.length > 0 ? (
        <ul className="list-none p-0 m-0 grid gap-4">
          {devices.map((device) => (
            <li
              key={device.id}
              data-testid={`device-${device.id}`}
              className="bg-white border border-border rounded-lg p-5 shadow-sm hover:shadow-md transition-shadow"
            >
              <div className="flex items-start justify-between gap-4 mb-3">
                <h3 className="text-base font-semibold text-navy m-0">{device.name}</h3>
                <div className="flex items-center gap-2">
                  <StatusBadge status={device.online ? "online" : "offline"} />
                  <StatusBadge status={device.status} />
                </div>
              </div>
              <dl className="grid grid-cols-[160px_1fr] gap-x-4 gap-y-2 text-sm max-md:grid-cols-[1fr]">
                <dt className="font-semibold text-navy pt-1">Hostname</dt>
                <dd className="text-text-secondary pt-1 break-words" data-testid="device-hostname">
                  {device.hostname ?? "Unavailable"}
                </dd>
                <dt className="font-semibold text-navy pt-1">Platform</dt>
                <dd className="text-text-secondary pt-1 break-words" data-testid="device-platform">
                  {device.platform ?? "Unavailable"}
                </dd>
                <dt className="font-semibold text-navy pt-1">Bridge version</dt>
                <dd className="text-text-secondary pt-1 break-words" data-testid="device-bridge-version">
                  {device.bridge_version ?? "Unavailable"}
                </dd>
                <dt className="font-semibold text-navy pt-1">Last heartbeat</dt>
                <dd className="text-text-secondary pt-1 break-words" data-testid="device-last-heartbeat">
                  {device.last_heartbeat_at?.slice(0, 19).replace("T", " ") ?? "Unavailable"}
                </dd>
                <dt className="font-semibold text-navy pt-1">Official MCP state</dt>
                <dd className="text-text-secondary pt-1 break-words" data-testid="device-mcp-state">
                  {device.official_mcp_state ? (
                    <StatusBadge status={device.official_mcp_state} />
                  ) : (
                    "Unavailable"
                  )}
                </dd>
                <dt className="font-semibold text-navy pt-1">Reconnect count</dt>
                <dd className="text-text-secondary pt-1 break-words" data-testid="device-reconnect-count">
                  {device.reconnect_count}
                </dd>
                <dt className="font-semibold text-navy pt-1">Last error</dt>
                <dd className="text-text-secondary pt-1 break-words" data-testid="device-last-error">
                  {device.last_error ?? "None reported"}
                </dd>
                <dt className="font-semibold text-navy pt-1">Enrollment</dt>
                <dd className="text-text-secondary pt-1 break-words">
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
                  className="mt-4"
                >
                  <label
                    htmlFor={`rename-${device.id}`}
                    className="block text-[13px] font-semibold text-navy mb-1"
                  >
                    Device name
                  </label>
                  <input
                    id={`rename-${device.id}`}
                    name="name"
                    value={draftName}
                    onChange={(event) => setDraftName(event.target.value)}
                    className="font-sans text-[15px] text-navy bg-white border border-border rounded-md px-3 py-2 w-full max-w-[400px] transition-colors focus:outline-none focus:border-red focus:shadow-[0_0_0_3px_var(--color-red-light)]"
                  />
                  <div className="flex gap-2 mt-3">
                    <button
                      type="submit"
                      disabled={busy || draftName.trim().length === 0}
                      className="px-4 py-2 text-sm font-medium bg-red text-white border border-red rounded-md hover:bg-red-hover disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
                    >
                      Save name
                    </button>
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => {
                        setRenaming(null);
                        setDraftName("");
                      }}
                      className="px-4 py-2 text-sm font-medium border border-border rounded-md text-navy bg-transparent hover:bg-surface-alt transition-colors"
                    >
                      Cancel rename
                    </button>
                  </div>
                </form>
              ) : (
                <div className="flex gap-2 flex-wrap mt-4">
                  <button
                    type="button"
                    onClick={() => {
                      setRenaming(device);
                      setDraftName(device.name);
                    }}
                    className="px-4 py-2 text-sm font-medium border border-border rounded-md text-navy bg-transparent hover:bg-surface-alt transition-colors"
                  >
                    Rename
                  </button>
                  {device.status !== "revoked" ? (
                    <>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => {
                          setRotatedCredential(null);
                          setRotating(device);
                        }}
                        className="px-4 py-2 text-sm font-medium border border-border rounded-md text-navy bg-transparent hover:bg-surface-alt transition-colors"
                      >
                        Rotate credential
                      </button>
                      <button
                        type="button"
                        onClick={() => setRevoking(device)}
                        className="px-4 py-2 text-sm font-medium border border-red rounded-md text-red bg-transparent hover:bg-error-bg transition-colors"
                      >
                        Revoke device
                      </button>
                    </>
                  ) : null}
                </div>
              )}
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