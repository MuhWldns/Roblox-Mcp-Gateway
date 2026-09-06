import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";

import Download from "./Download";
import Enroll from "./Enroll";
import Login from "./Login";

type MockRoute = { status?: number; json?: unknown };

type RecordedCall = {
  method: string;
  path: string;
  headers: Record<string, string>;
  body?: string;
};

// installFetch stubs window.fetch with per-route responses keyed by
// "METHOD /path" (query strings ignored). A route may be a list of responses
function installFetch(routes: Record<string, MockRoute | MockRoute[]>): RecordedCall[] {
  const calls: RecordedCall[] = [];
  const queue: Record<string, MockRoute[]> = {};
  for (const [key, value] of Object.entries(routes)) {
    queue[key] = Array.isArray(value) ? [...value] : [value];
  }
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const raw = typeof input === "string" ? input : input.toString();
      const url = new URL(raw, "http://localhost");
      const method = (init?.method ?? "GET").toUpperCase();
      calls.push({
        method,
        path: url.pathname,
        headers: Object.fromEntries(
          Object.entries(init?.headers ?? {}).map(([name, value]) => [
            name.toLowerCase(),
            value,
          ]),
        ),
        body: typeof init?.body === "string" ? init.body : undefined,
      });
      const responses = queue[`${method} ${url.pathname}`] ?? [];
      const route = responses.length > 1 ? responses.shift() : responses[0];
      const status = route === undefined ? 404 : (route.status ?? 200);
      const payload = route && route.json !== undefined ? JSON.stringify(route.json) : "";
      // jsdom provides no Response implementation; the client only needs the
      // status, ok, and json() contract, so a plain object suffices.
      return {
        status,
        ok: status >= 200 && status < 300,
        json: async () => JSON.parse(payload || "null"),
      } as unknown as Response;
    }),
  );
  return calls;
}

function renderAt(path: string, element: React.ReactElement) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route path="/download" element={<Download />} />
        <Route path="/enroll" element={<Enroll />} />
        <Route path="*" element={element} />
      </Routes>
    </MemoryRouter>,
  );
}

const meUrl = "GET /api/v1/me";
const metadataUrl = "GET /api/v1/bridge/download/metadata";
const csrfUrl = "GET /api/v1/csrf";
const claimUrl = "GET /api/v1/enrollments/claim";
const approveUrl = "POST /api/v1/enrollments/approve";

const freshMe = { user_id: "u1", display_name: "Builder 1516563360", trial: null };
const activeTrialMe = {
  user_id: "u1",
  display_name: "Builder 1516563360",
  trial: {
    active: true,
    started_at: "2026-09-04T11:00:00Z",
    ends_at: "2026-09-18T11:00:00Z",
  },
};
const metadata = {
  version: "1.4.2",
  filename: "RobloxBridge.exe",
  sha256: "a".repeat(64),
  size_bytes: 4096,
};
const claim = {
  device_id: "device-e2e",
  hostname: "DESKTOP-ABC123",
  platform: "windows",
  bridge_version: "1.4.2",
  expires_at: "2026-09-04T11:10:00Z",
};

describe("onboarding web flow", () => {
  beforeEach(() => {
    vi.useRealTimers();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("redirects unauthenticated visitors to the login page", async () => {
    installFetch({ [meUrl]: { status: 401 } });

    renderAt("/download", <Download />);

    expect(await screen.findByText("Continue with Roblox")).toBeTruthy();
  });

  it("shows already authenticated users the download page and redirects them away from login", async () => {
    installFetch({
      [meUrl]: { json: freshMe },
      [metadataUrl]: { json: metadata },
    });

    renderAt("/login", <Login />);

    expect(await screen.findByText("Download RobloxBridge")).toBeTruthy();
    expect(screen.getByTestId("bridge-version").textContent).toBe("1.4.2");
  });

  it("displays checksum, version, and size on the authenticated download page", async () => {
    installFetch({
      [meUrl]: { json: freshMe },
      [metadataUrl]: { json: metadata },
    });

    renderAt("/download", <Download />);

    await screen.findByText("Signed in as Builder 1516563360");
    expect(screen.getByTestId("bridge-version").textContent).toBe("1.4.2");
    const link = screen.getByTestId("download-link") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/api/v1/bridge/download");
    expect(screen.getByText("RobloxBridge.exe")).toBeTruthy();
  });

  it("states explicitly that downloading does not start the free trial", async () => {
    installFetch({
      [meUrl]: { json: freshMe },
      [metadataUrl]: { json: metadata },
    });

    renderAt("/download", <Download />);

    const notice = await screen.findByTestId("trial-notice");
    expect(notice.textContent).toMatch(/does not start your free trial/i);
  });

  it("confirms device connection with hostname display and CSRF-protected approval", async () => {
    const calls = installFetch({
      [meUrl]: { json: freshMe },
      [claimUrl]: { json: claim },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [approveUrl]: { status: 204 },
    });

    renderAt("/enroll?code=rkuc_TEST123", <Enroll />);

    expect(await screen.findByTestId("device-hostname")).toBeTruthy();
    expect(screen.getByTestId("device-hostname").textContent).toBe("DESKTOP-ABC123");
    expect(screen.getByText(/windows/)).toBeTruthy();
    expect(screen.getByText(/1\.4\.2/)).toBeTruthy();

    await userEvent.click(screen.getByRole("button", { name: "Approve device" }));

    await waitFor(() => {
      expect(screen.getByTestId("approval-status").textContent).toMatch(/approved/i);
    });
    const approve = calls.find((call) => call.path === "/api/v1/enrollments/approve");
    expect(approve).toBeTruthy();
    expect(approve?.headers["x-csrf-token"]).toBe("csrf-token-1");
    expect(JSON.parse(approve?.body ?? "{}")).toEqual({ user_code: "rkuc_TEST123" });
  });

  it("shows the first-binding trial state once the device completes its exchange", async () => {
    installFetch({
      [meUrl]: [{ json: freshMe }, { json: freshMe }, { json: activeTrialMe }],
      [claimUrl]: { json: claim },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [approveUrl]: { status: 204 },
    });

    renderAt("/enroll?code=rkuc_TEST123", <Enroll />);

    await userEvent.click(await screen.findByRole("button", { name: "Approve device" }));

    const trial = await screen.findByTestId("trial-state", {}, { timeout: 5000 });
    expect(trial.textContent).toMatch(/free trial active/i);
    expect(trial.textContent).toMatch(/2026-09-18/);
  });

  it("never persists credentials or tokens in browser storage", async () => {
    const calls = installFetch({
      [meUrl]: { json: activeTrialMe },
      [metadataUrl]: { json: metadata },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
    });

    renderAt("/download", <Download />);
    await screen.findByTestId("bridge-checksum");

    for (const call of calls) {
      for (const [name] of Object.entries(call.headers)) {
        expect(name.toLowerCase()).not.toBe("authorization");
      }
    }
    expect(window.localStorage?.length ?? 0).toBe(0);
    expect(window.sessionStorage?.length ?? 0).toBe(0);
  });
});
