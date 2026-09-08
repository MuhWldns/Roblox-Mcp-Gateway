import { getDevices, getStudios, getConnectors, type DeviceView, type StudioView, type ConnectorView } from "./client";

export interface SetupStatus {
  devices: DeviceView[];
  studios: StudioView[];
  connectors: ConnectorView[];
  ready: boolean;
  configured: boolean;
}

export async function getSetupStatus(): Promise<SetupStatus> {
  const [deviceData, studioData, connectorData] = await Promise.all([getDevices(), getStudios(), getConnectors()]);
  const devices = deviceData.devices.filter(device => device.status === "active");
  const studios = studioData.studios.filter(studio => studio.status === "active" && !studio.ended_at && devices.some(device => device.id === studio.device_id && device.online));
  const connectors = connectorData.connectors.filter(connector => !connector.revoked_at && devices.some(device => device.id === connector.device_id));
  const ready = connectors.some(connector => {
    const targets = studios.filter(studio => studio.device_id === connector.device_id);
    return connector.studio_session_id ? targets.some(studio => studio.id === connector.studio_session_id) : targets.length === 1;
  });
  return { devices, studios, connectors, ready, configured: devices.length > 0 && connectors.length > 0 };
}

export async function getSetupDestination() {
  return (await getSetupStatus()).configured ? "/dashboard" : "/setup";
}
