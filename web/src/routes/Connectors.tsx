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
      setTargets({});
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

  const activeDevices =
    devices !== null ? devices.filter((device) => device.status === "active") : [];

  function targetOf(connector: ConnectorView): ConnectorTarget {
    const edited = targets[connector.id];
    if (edited !== undefined) {
      return edited;
    }
    return {
      deviceId: connector.device_id,
      studioSessionId: connector.studio_session_id ?? "",
    };
  }

  function activeStudiosOn(deviceId: string): StudioView[] {
    if (studios === null) {
      return [];
    }
    return studios.filter(
      (studio) => studio.status === "active" && studio.device_id === deviceId,
    );
  }

  function deviceName(deviceId: string): string {
    const device = devices?.find((entry) => entry.id === deviceId);
    return device !== undefined ? device.name : deviceId;
  }

  function studioLabel(studioSessionId: string): string {
    const studio = studios?.find((entry) => entry.id === studioSessionId);
    return studio !== undefined ? studio.studio_id : studioSessionId;
  }

  // A grant with a device but no explicit Studio cannot route while several
  // Studio sessions are live on that device — the resolver refuses to guess.
  function ambiguousStudioCount(connector: ConnectorView): number {
    if (connector.studio_session_id !== null || connector.device_id === "") {
      return 0;
    }
    return activeStudiosOn(connector.device_id).length;
  }

  async function saveTarget(connector: ConnectorView) {
    const target = targetOf(connector);
    if (target.deviceId === "") {
      setActionError("Pick a device before saving the target.");
      return;
    }
    setBusy(true);
    try {
      await setConnectorTarget(connector.id, target.deviceId, target.studioSessionId);
      setActionError(null);
      await load();
    } catch (error) {
      if (error instanceof UnauthorizedError) {
        setDenied(true);
      } else {
        setActionError("Saving the target failed. Please try again.");
      }
    } finally {
      setBusy(false);
    }
  }

  async function confirmRevoke() {
    if (revoking === null) {
      return;
    }
    setBusy(true);
    try {
      await revokeConnector(revoking.id);
      setRevoking(null);
      setActionError(null);
      await load();
    } catch (error) {
      setRevoking(null);
      if (error instanceof UnauthorizedError) {
        setDenied(true);
      } else {
        setActionError("Revoking the connector failed. Please try again.");
      }
    } finally {
      setBusy(false);
    }
  }

  if (denied) {
    return <Navigate to="/login" replace />;
  }

  return (
    <section data-testid="page-connectors" aria-labelledby="connectors-title">
      <h2 id="connectors-title">Connectors</h2>
      <p>AI connectors authorized to reach your Studio through the gateway.</p>
      {actionError ? <p role="alert">{actionError}</p> : null}
      {failed ? (
        <p role="alert">Connectors unavailable right now. Reload to try again.</p>
      ) : null}
      {connectors === null && !failed ? <p role="status">Loading connectors…</p> : null}
      {connectors !== null && connectors.length === 0 ? (
        <p>
          No connectors yet. Add the RobloxKit MCP server in ChatGPT or Claude
          and finish their authorization to see the grant here.
        </p>
      ) : null}
      {connectors !== null && connectors.length > 0 ? (
        <ul>
          {connectors.map((connector) => {
            const target = targetOf(connector);
            const candidateStudios = activeStudiosOn(target.deviceId);
            const ambiguousCount = ambiguousStudioCount(connector);
            return (
              <li key={connector.id} data-testid={`connector-${connector.id}`}>
                <h3>{connector.client_name}</h3>
                <p>{connector.client_id}</p>
                <p data-testid={`connector-scopes-${connector.id}`}>
                  Scopes: {connector.scopes.join(", ")}
                </p>
                <p>Resource: {connector.resource}</p>
                <p data-testid={`connector-target-${connector.id}`}>
                  Target: {deviceName(connector.device_id)}
                  {connector.studio_session_id !== null
                    ? ` · Studio session ${studioLabel(connector.studio_session_id)}`
                    : " · no Studio chosen"}
                </p>
                <p>Authorized {connector.created_at.slice(0, 10)}</p>
                {connector.revoked_at !== null ? (
                  <p>
                    <StatusBadge status="revoked" /> on{" "}
                    {connector.revoked_at.slice(0, 10)}. Its tokens stopped
                    working immediately.
                  </p>
                ) : (
                  <>
                    {ambiguousCount >= 2 ? (
                      <p role="alert" data-testid={`connector-ambiguity-${connector.id}`}>
                        Requests through this connector are blocked:{" "}
                        {ambiguousCount} Studio sessions are active on{" "}
                        {deviceName(connector.device_id)} and no Studio is
                        chosen. The gateway refuses to guess which Studio to
                        use — pick one below.
                      </p>
                    ) : null}
                    <form
                      onSubmit={(event) => {
                        event.preventDefault();
                        void saveTarget(connector);
                      }}
                    >
                      <label htmlFor={`target-device-${connector.id}`}>Device</label>
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
                        >
                          {activeDevices.map((device) => (
                            <option key={device.id} value={device.id}>
                              {device.name}
                            </option>
                          ))}
                        </select>
                      ) : (
                        <p>No active devices to target.</p>
                      )}
                      <label htmlFor={`target-studio-${connector.id}`}>
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
                      >
                        <option value="">None — this device only</option>
                        {candidateStudios.map((studio) => (
                          <option key={studio.id} value={studio.id}>
                            {studio.studio_id}
                          </option>
                        ))}
                      </select>
                      <button
                        type="submit"
                        disabled={busy || target.deviceId === "" || activeDevices.length === 0}
                      >
                        Save target
                      </button>
                    </form>
                    <button type="button" onClick={() => setRevoking(connector)}>
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
