import { useCallback, useEffect, useState } from "react";
import { Navigate } from "react-router";
import {
  type ConnectorView,
  type DeviceView,
  type StudioView,
  UnauthorizedError,
  getConnectors,
  getDevices,
  getStudios,
  revokeConnector,
  setConnectorTarget,
} from "../api/client";
import ConfirmDialog from "../components/ConfirmDialog";
import StatusBadge from "../components/StatusBadge";

type ConnectorTarget = { deviceId: string; studioSessionId: string };

// Connectors lists the AI connectors authorized through the gateway's OAuth
// flow: their granted scopes, the device/Studio target they route to, and
// the self-service target change and revocation. Requests through a grant
// with a device but no explicit Studio are blocked while more than one
// Studio session is active — the chooser resolves the ambiguity.
export default function Connectors() {
  const [connectors, setConnectors] = useState<ConnectorView[] | null>(null);
  const [devices, setDevices] = useState<DeviceView[] | null>(null);
  const [studios, setStudios] = useState<StudioView[] | null>(null);
  const [targets, setTargets] = useState<Record<string, ConnectorTarget>>({});
  const [denied, setDenied] = useState(false);
  const [failed, setFailed] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<ConnectorView | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const [connectorList, deviceList, studioList] = await Promise.all([
        getConnectors(),
        getDevices(),
        getStudios(),
      ]);
      setConnectors(connectorList.connectors);
      setDevices(deviceList.devices);
      setStudios(studioList.studios);
      setFailed(false);
      setActionError(null);
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

  const activeDevices =
    devices !== null ? devices.filter((device) => device.status === "active") : [];

  function targetOf(connector: ConnectorView): ConnectorTarget {
    const current = targets[connector.id];
    if (current) return current;
    return {
      deviceId: connector.device_id,
      studioSessionId: connector.studio_session_id ?? "",
    };
  }

  function activeStudiosOn(deviceId: string): StudioView[] {
    if (studios === null) return [];
    return studios.filter(
      (studio) => studio.device_id === deviceId && studio.status === "active",
    );
  }

  function deviceName(deviceId: string): string {
    if (devices === null) return deviceId;
    const device = devices.find((d) => d.id === deviceId);
    return device !== undefined ? device.name : deviceId;
  }

  function studioLabel(studioSessionId: string): string {
    if (studios === null) return studioSessionId;
    const studio = studios.find((s) => s.id === studioSessionId);
    return studio !== undefined ? studio.studio_id : studioSessionId;
  }

  // A grant with a device but no explicit Studio cannot route while several
  // Studio sessions are live on that device — the resolver refuses to guess.
  function ambiguousStudioCount(connector: ConnectorView): number {
    if (connector.studio_session_id !== null) return 0;
    const target = targetOf(connector);
    if (target.studioSessionId !== "") return 0;
    return activeStudiosOn(target.deviceId).length;
  }

  async function saveTarget(connector: ConnectorView) {
    const target = targetOf(connector);
    setBusy(true);
    setActionError(null);
    try {
      await setConnectorTarget(
        connector.id,
        target.deviceId,
        target.studioSessionId !== "" ? target.studioSessionId : "",
      );
      await load();
    } catch (error: unknown) {
      setActionError(
        error instanceof Error ? error.message : "Save failed. Try again.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function confirmRevoke() {
    if (revoking === null) return;
    setBusy(true);
    setActionError(null);
    try {
      await revokeConnector(revoking.id);
      setRevoking(null);
      await load();
    } catch (error: unknown) {
      setActionError(
        error instanceof Error ? error.message : "Revoke failed. Try again.",
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
      data-testid="page-connectors"
      aria-labelledby="connectors-title"
      className="animate-[pageEnter_200ms_ease]"
    >
      <h2 id="connectors-title" className="text-xl font-semibold text-navy mb-1">
        Connectors
      </h2>
      <p className="text-text-secondary mb-6">
        AI connectors authorized to reach your Studio through the gateway.
      </p>
      {actionError ? (
        <div role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium mb-4">
          {actionError}
        </div>
      ) : null}
      {failed ? (
        <div role="alert" className="bg-error-bg text-red border border-red rounded-md px-4 py-3 text-sm font-medium mb-4">
          Connectors unavailable right now. Reload to try again.
        </div>
      ) : null}
      {connectors === null && !failed ? (
        <p role="status" className="text-text-muted italic">Loading connectors…</p>
      ) : null}
      {connectors !== null && connectors.length === 0 ? (
        <div className="text-center py-12 px-6 bg-white border-2 border-dashed border-border rounded-lg">
          <p className="text-text-muted">
            No connectors yet. Add the RobloxKit MCP server in ChatGPT or Claude
            and finish their authorization to see the grant here.
          </p>
        </div>
      ) : null}
      {connectors !== null && connectors.length > 0 ? (
        <ul className="list-none p-0 m-0 grid gap-4">
          {connectors.map((connector) => {
            const target = targetOf(connector);
            const candidateStudios = activeStudiosOn(target.deviceId);
            const ambiguousCount = ambiguousStudioCount(connector);
            return (
              <li
                key={connector.id}
                data-testid={`connector-${connector.id}`}
                className="bg-white border border-border rounded-lg p-5 shadow-sm hover:shadow-md transition-shadow"
              >
                <div className="flex items-start justify-between gap-4 mb-3">
                  <h3 className="text-base font-semibold text-navy m-0">
                    {connector.client_name}
                  </h3>
                </div>
                <p className="text-sm text-text-secondary mb-2">
                  {connector.client_id}
                </p>
                <p className="text-sm text-text-secondary mb-2" data-testid={`connector-scopes-${connector.id}`}>
                  Scopes: {connector.scopes.join(", ")}
                </p>
                <p className="text-sm text-text-secondary mb-2">
                  Resource: {connector.resource}
                </p>
                <p className="text-sm text-text-secondary mb-2" data-testid={`connector-target-${connector.id}`}>
                  Target: {deviceName(connector.device_id)}
                  {connector.studio_session_id !== null
                    ? ` · Studio session ${studioLabel(connector.studio_session_id)}`
                    : " · no Studio chosen"}
                </p>
                <p className="text-sm text-text-secondary mb-2">
                  Authorized {connector.created_at.slice(0, 10)}
                </p>
                {connector.revoked_at !== null ? (
                  <p className="text-sm text-text-secondary">
                    <StatusBadge status="revoked" /> on{" "}
                    {connector.revoked_at.slice(0, 10)}. Its tokens stopped
                    working immediately.
                  </p>
                ) : (
                  <>
                    {ambiguousCount >= 2 ? (
                      <div
                        role="alert"
                        data-testid={`connector-ambiguity-${connector.id}`}
                        className="bg-warning-bg text-warning border border-warning rounded-md px-4 py-3 text-sm font-medium mb-4"
                      >
                        Requests through this connector are blocked:{" "}
                        {ambiguousCount} Studio sessions are active on{" "}
                        {deviceName(connector.device_id)} and no Studio is
                        chosen. The gateway refuses to guess which Studio to
                        use — pick one below.
                      </div>
                    ) : null}
                    <div className="bg-surface-alt border border-border-light rounded-md p-4 my-3">
                      <form
                        onSubmit={(event) => {
                          event.preventDefault();
                          void saveTarget(connector);
                        }}
                        className="space-y-3"
                      >
                        <div>
                          <label
                            htmlFor={`target-device-${connector.id}`}
                            className="block text-[13px] font-semibold text-navy mb-1"
                          >
                            Device
                          </label>
                          {activeDevices.length > 0 ? (
                            <select
                              id={`target-device-${connector.id}`}
                              value={target.deviceId}
                              onChange={(event) =>
                                setTargets((previous) => ({
                                  ...previous,
                                  [connector.id]: {
                                    deviceId: event.target.value,
                                    studioSessionId: "",
                                  },
                                }))
                              }
                              className="w-full max-w-full font-sans text-[15px] text-navy bg-white border border-border rounded-md px-3 py-2 transition-colors focus:outline-none focus:border-red focus:shadow-[0_0_0_3px_var(--color-red-light)]"
                            >
                              {activeDevices.map((device) => (
                                <option key={device.id} value={device.id}>
                                  {device.name}
                                </option>
                              ))}
                            </select>
                          ) : (
                            <p className="text-sm text-text-muted">
                              No active devices to target.
                            </p>
                          )}
                        </div>
                        <div>
                          <label
                            htmlFor={`target-studio-${connector.id}`}
                            className="block text-[13px] font-semibold text-navy mb-1"
                          >
                            Studio session
                          </label>
                          <select
                            id={`target-studio-${connector.id}`}
                            value={target.studioSessionId}
                            onChange={(event) =>
                              setTargets((previous) => ({
                                ...previous,
                                [connector.id]: {
                                  deviceId: target.deviceId,
                                  studioSessionId: event.target.value,
                                },
                              }))
                            }
                            className="w-full max-w-full font-sans text-[15px] text-navy bg-white border border-border rounded-md px-3 py-2 transition-colors focus:outline-none focus:border-red focus:shadow-[0_0_0_3px_var(--color-red-light)]"
                          >
                            <option value="">None — this device only</option>
                            {candidateStudios.map((studio) => (
                              <option key={studio.id} value={studio.id}>
                                {studio.studio_id}
                              </option>
                            ))}
                          </select>
                        </div>
                        <button
                          type="submit"
                          disabled={
                            busy ||
                            target.deviceId === "" ||
                            activeDevices.length === 0
                          }
                          className="px-4 py-2 text-sm font-medium bg-red text-white border border-red rounded-md hover:bg-red-hover disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
                        >
                          Save target
                        </button>
                      </form>
                    </div>
                    <button
                      type="button"
                      onClick={() => setRevoking(connector)}
                      className="px-4 py-2 text-sm font-medium border border-red rounded-md text-red bg-transparent hover:bg-error-bg transition-colors"
                    >
                      Revoke connector
                    </button>
                  </>
                )}
              </li>
            );
          })}
        </ul>
      ) : null}
      {revoking !== null ? (
        <ConfirmDialog
          title="Revoke this connector?"
          body={
            <>
              <p>
                Revoking <strong>{revoking.client_name}</strong> makes its access
                and refresh tokens stop working immediately. Re-authorizing the
                connector from ChatGPT or Claude creates a fresh grant.
              </p>
            </>
          }
          confirmLabel="Yes, revoke this connector"
          busy={busy}
          onConfirm={() => void confirmRevoke()}
          onCancel={() => setRevoking(null)}
        />
      ) : null}
    </section>
  );
}