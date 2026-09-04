// Browser API client: every call uses relative URLs with credentials so
// cookies ride along. No credential, session, device, or MCP token is ever
// received by these endpoints, and nothing is written to localStorage or
// sessionStorage — the CSRF token lives in module memory only.

export interface TrialState {
  active: boolean;
  started_at: string;
  ends_at: string;
}

export interface MeResponse {
  user_id: string;
  display_name: string;
  trial: TrialState | null;
}

export interface CsrfResponse {
  csrf_token: string;
}

export interface DownloadMetadata {
  version: string;
  filename: string;
  sha256: string;
  size_bytes: number;
}

export interface EnrollmentClaim {
  device_id: string;
  hostname: string;
  platform: string;
  bridge_version: string;
  expires_at: string;
}

export interface DeviceView {
  id: string;
  name: string;
  status: string;
  online: boolean;
  created_at: string;
  updated_at: string;
}

export interface DevicesResponse {
  devices: DeviceView[];
}

export interface StudioView {
  id: string;
  device_id: string;
  studio_id: string;
  status: string;
  started_at: string;
  ended_at: string | null;
}

export interface StudiosResponse {
  studios: StudioView[];
}

export interface ConnectorView {
  id: string;
  client_id: string;
  client_name: string;
  scopes: string[];
  resource: string;
  device_id: string;
  studio_session_id: string | null;
  created_at: string;
  revoked_at: string | null;
}

export interface ConnectorsResponse {
  connectors: ConnectorView[];
}

export interface LicenseState {
  status: string;
  device_slots: number;
  active_bindings: number;
}

export interface LicenseResponse {
  trial: TrialState | null;
  license: LicenseState | null;
}

export interface DiagnosticsResponse {
  database: string;
  devices_registered: number;
  devices_online: number;
  studio_sessions_active: number;
}

// ServerClock anchors time arithmetic: serverNowMs is the server's wall clock
// from the response Date header (the Go server always sends one), and
// receivedAtMs is the client instant of receipt used only for the elapsed
// delta. The client's absolute clock is never the countdown anchor.
export interface ServerClock {
  serverNowMs: number | null;
  receivedAtMs: number;
}

export interface MeSnapshot {
  me: MeResponse;
  clock: ServerClock;
}

export interface LicenseSnapshot {
  license: LicenseResponse;
  clock: ServerClock;
}

export class UnauthorizedError extends Error {
  constructor() {
    super("authentication required");
    this.name = "UnauthorizedError";
  }
}

let csrfToken: string | null = null;

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await fetch(path, { ...init, credentials: "include" });
  if (response.status === 401) {
    throw new UnauthorizedError();
  }
  if (!response.ok) {
    throw new Error(`request failed with HTTP ${response.status}`);
  }
  if (response.status === 204) {
    return undefined as T;
  }
  return (await response.json()) as T;
}

// requestSnapshot fetches a read that also carries the server's clock, so
// countdowns anchor to the server's "now" instead of the browser's.
async function requestSnapshot<T>(path: string): Promise<{ data: T; clock: ServerClock }> {
  const response = await fetch(path, { credentials: "include" });
  if (response.status === 401) {
    throw new UnauthorizedError();
  }
  if (!response.ok) {
    throw new Error(`request failed with HTTP ${response.status}`);
  }
  // The Go server stamps every response with its own clock. Stripped proxies
  // and test doubles may omit the header; the countdown then degrades to the
  // client clock instead of failing the whole screen.
  const headers = response.headers as Pick<Headers, "get"> | undefined;
  const dateHeader = typeof headers?.get === "function" ? headers.get("date") : null;
  const parsedDate = dateHeader === null ? Number.NaN : Date.parse(dateHeader);
  const clock: ServerClock = {
    serverNowMs: Number.isNaN(parsedDate) ? null : parsedDate,
    receivedAtMs: Date.now(),
  };
  if (response.status === 204) {
    return { data: undefined as T, clock };
  }
  const data = (await response.json()) as T;
  return { data, clock };
}

export async function getMe(): Promise<MeResponse> {
  return request<MeResponse>("/api/v1/me");
}

async function ensureCsrf(): Promise<string> {
  if (csrfToken === null) {
    const issued = await request<CsrfResponse>("/api/v1/csrf");
    csrfToken = issued.csrf_token;
  }
  return csrfToken;
}

export async function getDownloadMetadata(): Promise<DownloadMetadata> {
  return request<DownloadMetadata>("/api/v1/bridge/download/metadata");
}

export async function getEnrollmentClaim(code: string): Promise<EnrollmentClaim> {
  const suffix = `?code=${encodeURIComponent(code)}`;
  return request<EnrollmentClaim>(`/api/v1/enrollments/claim${suffix}`);
}

export async function approveEnrollment(code: string): Promise<void> {
  const token = await ensureCsrf();
  return request<void>("/api/v1/enrollments/approve", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-CSRF-Token": token,
    },
    body: JSON.stringify({ user_code: code }),
  });
}

export async function logout(): Promise<void> {
  const token = await ensureCsrf();
  try {
    await request<void>("/api/v1/auth/logout", {
      method: "POST",
      headers: { "X-CSRF-Token": token },
    });
  } finally {
    // The session is gone; the token minted for it must not leak into the
    // next sign-in, so the next mutation fetches a fresh CSRF token.
    csrfToken = null;
  }
}

export async function getMeSnapshot(): Promise<MeSnapshot> {
  const { data, clock } = await requestSnapshot<MeResponse>("/api/v1/me");
  return { me: data, clock };
}

// trialDaysRemaining derives the remaining whole days from the server's
// timestamps only: the server clock anchors "now" and the client clock
// contributes nothing but the elapsed time since the response arrived.
export function trialDaysRemaining(endsAt: string, clock: ServerClock): number {
  const endsMs = Date.parse(endsAt);
  if (Number.isNaN(endsMs)) {
    return 0;
  }
  const nowMs =
    clock.serverNowMs !== null
      ? clock.serverNowMs + Math.max(0, Date.now() - clock.receivedAtMs)
      : Date.now();
  const remainingMs = endsMs - nowMs;
  if (remainingMs <= 0) {
    return 0;
  }
  return Math.ceil(remainingMs / 86_400_000);
}

// mutation posts a session-bound change with the CSRF double-submit pair; a
// body is sent only when the endpoint expects one.
async function mutation(path: string, body?: string): Promise<void> {
  const token = await ensureCsrf();
  return request<void>(path, {
    method: "POST",
    headers: {
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      "X-CSRF-Token": token,
    },
    ...(body === undefined ? {} : { body }),
  });
}

export async function getDevices(): Promise<DevicesResponse> {
  return request<DevicesResponse>("/api/v1/devices");
}

export async function renameDevice(deviceId: string, name: string): Promise<void> {
  return mutation(
    `/api/v1/devices/${encodeURIComponent(deviceId)}/rename`,
    JSON.stringify({ name }),
  );
}

export async function revokeDevice(deviceId: string): Promise<void> {
  return mutation(`/api/v1/devices/${encodeURIComponent(deviceId)}/revoke`);
}

export async function getStudios(): Promise<StudiosResponse> {
  return request<StudiosResponse>("/api/v1/studios");
}

export async function getConnectors(): Promise<ConnectorsResponse> {
  return request<ConnectorsResponse>("/api/v1/connectors");
}

export async function setConnectorTarget(
  grantId: string,
  deviceId: string,
  studioSessionId: string,
): Promise<void> {
  return mutation(
    `/api/v1/connectors/${encodeURIComponent(grantId)}/target`,
    JSON.stringify({ device_id: deviceId, studio_session_id: studioSessionId }),
  );
}

export async function revokeConnector(grantId: string): Promise<void> {
  return mutation(`/api/v1/connectors/${encodeURIComponent(grantId)}/revoke`);
}

export async function getLicenseSnapshot(): Promise<LicenseSnapshot> {
  const { data, clock } = await requestSnapshot<LicenseResponse>("/api/v1/license");
  return { license: data, clock };
}

export async function getDiagnostics(): Promise<DiagnosticsResponse> {
  return request<DiagnosticsResponse>("/api/v1/diagnostics");
}
