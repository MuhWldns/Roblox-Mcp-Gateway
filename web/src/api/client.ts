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
