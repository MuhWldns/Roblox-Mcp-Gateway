// Browser API client: every call uses relative URLs with credentials so
// cookies ride along. The one-time device credential returned by rotation is
// handed directly to the caller; no secret is written to localStorage or
// sessionStorage. The CSRF token lives in module memory only.

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
  hostname: string | null;
  platform: string | null;
  bridge_version: string | null;
  last_heartbeat_at: string | null;
  official_mcp_state: string | null;
  reconnect_count: number;
  last_error: string | null;
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

export interface LicenseOwner {
  roblox_id_masked: string;
  display_name: string;
}

export interface LicenseState {
  status: string;
  expires_at: string | null;
  subscription_id: string | null;
  device_slots: number;
  active_bindings: number;
  available_slots: number;
  allowed_scopes: string[];
  usage_limit: number | null;
  current_usage: number;
  transfer_status: string | null;
  recovery_status: string | null;
}

export interface LicenseResponse {
  owner: LicenseOwner;
  trial: TrialState | null;
  license: LicenseState | null;
}

export interface DiagnosticDeviceView {
  id: string;
  name: string;
  status: string;
  online: boolean;
  last_heartbeat_at: string | null;
  official_mcp_state: string | null;
  reconnect_count: number;
  last_error: string | null;
}

export interface DiagnosticsResponse {
  database: string;
  devices_registered: number;
  devices_online: number;
  studio_sessions_active: number;
  devices: DiagnosticDeviceView[];
}

export interface RotatedDeviceCredential {
  device_id: string;
  device_credential: string;
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
    // The status rides on the error so admin screens can explain 403/404/409
    // distinctly; the message stays safe to render anywhere.
    throw new ApiError(response.status);
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
async function mutation<T = void>(path: string, body?: string): Promise<T> {
  const token = await ensureCsrf();
  return request<T>(path, {
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

export async function rotateDeviceCredential(
  deviceId: string,
): Promise<RotatedDeviceCredential> {
  return mutation<RotatedDeviceCredential>(
    `/api/v1/devices/${encodeURIComponent(deviceId)}/rotate-credential`,
  );
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

// --- Administration surface -------------------------------------------------

// ApiError carries the HTTP status of a failed call. The message matches the
// historical generic format so nothing that renders error.message changes.
export class ApiError extends Error {
  readonly status: number;

  constructor(status: number) {
    super(`request failed with HTTP ${status}`);
    this.name = "ApiError";
    this.status = status;
  }
}

export interface AdminIdentityView {
  subject: string;
  display_name: string;
}

export interface AdminDeviceView {
  id: string;
  name: string;
  status: string;
  online: boolean;
  created_at: string;
  updated_at: string;
}

export interface AdminLicenseView {
  status: string;
  device_slots: number;
  active_bindings: number;
}

export interface AdminConnectorView {
  id: string;
  client_id: string;
  client_name: string;
  device_id: string;
  studio_session_id?: string;
  scopes: string[];
  created_at: string;
  revoked_at?: string;
}

export interface AdminTrialView {
  id: string;
  started_at: string;
  ends_at: string;
  active: boolean;
}

export interface AdminTransferPreview {
  user_id: string;
  identity: AdminIdentityView | null;
  devices: AdminDeviceView[];
  license: AdminLicenseView | null;
  version: string;
}

export interface AdminRecoveryPreview {
  user_id: string;
  identity: AdminIdentityView | null;
  devices: AdminDeviceView[];
  connectors: AdminConnectorView[];
  license: AdminLicenseView | null;
  version: string;
}

export interface AdminTrialPreview {
  user_id: string;
  identity: AdminIdentityView | null;
  trial: AdminTrialView | null;
  version: string;
}

export interface AdminTransferRequest {
  user_id: string;
  license_id: string;
  old_device_id: string;
  new_device_id: string;
  expected_version: string;
  case_id: string;
  reason: string;
  evidence_ref: string;
}

export interface AdminRecoveryRequest {
  user_id: string;
  expected_version: string;
  case_id: string;
  reason: string;
  evidence_ref: string;
  new_identity_id?: string;
}

export interface AdminExtensionRequest {
  user_id: string;
  entitlement_id: string;
  new_ends_at: string;
  expected_version: string;
  case_id: string;
  reason: string;
  evidence_ref: string;
}

export async function getAdminTransferPreview(userId: string): Promise<AdminTransferPreview> {
  return request<AdminTransferPreview>(
    `/api/v1/admin/users/${encodeURIComponent(userId)}/transfer-preview`,
  );
}

export async function getAdminRecoveryPreview(userId: string): Promise<AdminRecoveryPreview> {
  return request<AdminRecoveryPreview>(
    `/api/v1/admin/users/${encodeURIComponent(userId)}/recovery-preview`,
  );
}

export async function getAdminTrialPreview(userId: string): Promise<AdminTrialPreview> {
  return request<AdminTrialPreview>(
    `/api/v1/admin/users/${encodeURIComponent(userId)}/trial-preview`,
  );
}

export async function adminTransferDevice(payload: AdminTransferRequest): Promise<void> {
  return mutation("/api/v1/admin/transfers", JSON.stringify(payload));
}

export async function adminRecoverIdentity(payload: AdminRecoveryRequest): Promise<void> {
  return mutation("/api/v1/admin/recoveries", JSON.stringify(payload));
}

export async function adminExtendTrial(payload: AdminExtensionRequest): Promise<void> {
  return mutation("/api/v1/admin/trial-extensions", JSON.stringify(payload));
}
