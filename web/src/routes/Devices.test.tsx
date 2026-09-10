import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";

import ConfirmDialog from "../components/ConfirmDialog";
import StatusBadge from "../components/StatusBadge";
import Devices from "./Devices";
import Diagnostics from "./Diagnostics";
import Studios from "./Studios";

type MockRoute = { status?: number; json?: unknown };

type RecordedCall = {
  method: string;
  path: string;
  headers: Record<string, string>;
  body?: string;
};

type Deferred<T> = {
  promise: Promise<T>;
  resolve: (value: T | PromiseLike<T>) => void;
  reject: (reason?: unknown) => void;
};

function createDeferred<T = void>(): Deferred<T> {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

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
        <Route path="/download" element={<div />} />
        <Route path="/login" element={<div data-testid="login-page">Login Page</div>} />
        <Route path="*" element={element} />
      </Routes>
    </MemoryRouter>,
  );
}

const devicesUrl = "GET /api/v1/devices";
const renameUrl = "POST /api/v1/devices/device-1/rename";
const revokeUrl = "POST /api/v1/devices/device-1/revoke";
const rotateCredentialUrl = "POST /api/v1/devices/device-1/rotate-credential";
const studiosUrl = "GET /api/v1/studios";
const diagnosticsUrl = "GET /api/v1/diagnostics";
const csrfUrl = "GET /api/v1/csrf";

const deviceOnline = {
  id: "device-1",
  name: "Laptop A",
  status: "active",
  online: true,
  hostname: "studio-laptop",
  platform: "windows-amd64",
  bridge_version: "bridge-2026.09",
  last_heartbeat_at: "2026-09-04T10:59:30Z",
  official_mcp_state: "ready",
  reconnect_count: 3,
  last_error: "Connection reset; retry scheduled",
  created_at: "2026-09-01T10:00:00Z",
  updated_at: "2026-09-04T10:00:00Z",
};
const deviceOffline = {
  id: "device-2",
  name: "Desktop B",
  status: "active",
  online: false,
  hostname: null,
  platform: null,
  bridge_version: null,
  last_heartbeat_at: null,
  official_mcp_state: null,
  reconnect_count: 0,
  last_error: null,
  created_at: "2026-09-02T10:00:00Z",
  updated_at: "2026-09-03T10:00:00Z",
};
const deviceRevoked = {
  id: "device-3",
  name: "Old PC",
  status: "revoked",
  online: false,
  hostname: "old-pc",
  platform: "windows-amd64",
  bridge_version: "bridge-2026.08",
  last_heartbeat_at: "2026-09-03T08:59:00Z",
  official_mcp_state: "stopped",
  reconnect_count: 8,
  last_error: null,
  created_at: "2026-08-30T10:00:00Z",
  updated_at: "2026-09-03T09:00:00Z",
};
const studioActive = {
  id: "studio-1",
  device_id: "device-1",
  studio_id: "studio-alpha",
  status: "active",
  started_at: "2026-09-04T09:00:00Z",
  ended_at: null,
};
const studioEnded = {
  id: "studio-2",
  device_id: "device-1",
  studio_id: "studio-beta",
  status: "ended",
  started_at: "2026-09-03T09:00:00Z",
  ended_at: "2026-09-03T18:00:00Z",
};
const diagnostics = {
  database: "ok",
  devices_registered: 2,
  devices_online: 1,
  studio_sessions_active: 1,
  devices: [
    {
      id: "device-1",
      name: "Laptop A",
      status: "active",
      online: true,
      last_heartbeat_at: "2026-09-04T10:59:30Z",
      official_mcp_state: "ready",
      reconnect_count: 3,
      last_error: "Connection reset; retry scheduled",
    },
    {
      id: "device-2",
      name: "Desktop B",
      status: "active",
      online: false,
      last_heartbeat_at: null,
      official_mcp_state: null,
      reconnect_count: 0,
      last_error: null,
    },
  ],
};

describe("devices screen", () => {
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
    vi.unstubAllGlobals();
  });

  it("lists devices with text presence and status, not color-only markers", async () => {
    installFetch({
      [devicesUrl]: { json: { devices: [deviceOnline, deviceOffline, deviceRevoked] } },
    });

    renderAt("/devices", <Devices />);

    expect(await screen.findByTestId("page-devices")).toBeTruthy();
    await screen.findByText("Laptop A");
    expect(screen.getByText("Desktop B")).toBeTruthy();
    expect(screen.getByText("Old PC")).toBeTruthy();
    // Presence and lifecycle state are words in the DOM, readable without
    // any color perception.
    expect(within(screen.getByTestId("device-device-1")).getByText("Online")).toBeTruthy();
    expect(within(screen.getByTestId("device-device-1")).getByText("Active")).toBeTruthy();
    expect(within(screen.getByTestId("device-device-2")).getByText("Offline")).toBeTruthy();
    expect(within(screen.getByTestId("device-device-3")).getByText("Revoked")).toBeTruthy();
  });

  it("renders bridge identity and operational details with explicit unavailable states", async () => {
    installFetch({
      [devicesUrl]: { json: { devices: [deviceOnline, deviceOffline] } },
    });

    renderAt("/devices", <Devices />);

    const online = await screen.findByTestId("device-device-1");
    expect(within(online).getByTestId("device-hostname").textContent).toBe("studio-laptop");
    expect(within(online).getByTestId("device-platform").textContent).toBe("windows-amd64");
    expect(within(online).getByTestId("device-bridge-version").textContent).toBe(
      "bridge-2026.09",
    );
    expect(within(online).getByTestId("device-last-heartbeat").textContent).toContain(
      "2026-09-04",
    );
    expect(within(online).getByTestId("device-mcp-state").textContent).toMatch(/ready/i);
    expect(within(online).getByTestId("device-reconnect-count").textContent).toBe("3");
    expect(within(online).getByTestId("device-last-error").textContent).toBe(
      "Connection reset; retry scheduled",
    );

    const offline = screen.getByTestId("device-device-2");
    expect(within(offline).getByTestId("device-hostname").textContent).toBe("Unavailable");
    expect(within(offline).getByTestId("device-platform").textContent).toBe("Unavailable");
    expect(within(offline).getByTestId("device-bridge-version").textContent).toBe(
      "Unavailable",
    );
    expect(within(offline).getByTestId("device-last-heartbeat").textContent).toBe(
      "Unavailable",
    );
    expect(within(offline).getByTestId("device-mcp-state").textContent).toBe("Unavailable");
    expect(within(offline).getByTestId("device-last-error").textContent).toBe("None reported");
  });

  it("shows an empty state instead of a blank list before any enrollment", async () => {
    installFetch({ [devicesUrl]: { json: { devices: [] } } });
    renderAt("/devices", <Devices />);

    expect(await screen.findByText(/no devices/i)).toBeTruthy();
    expect(screen.getByText(/connect your first PC/i)).toBeTruthy();
  });

  it("keeps the section heading and surfaces an inline error when the initial API call fails", async () => {
    installFetch({ [devicesUrl]: { status: 500 } });

    renderAt("/devices", <Devices />);

    expect(await screen.findByText(/Devices unavailable/i)).toBeTruthy();
    expect(screen.getByTestId("page-devices").textContent).toMatch(/Devices/i);
  });

  it("renames a device through the CSRF-protected mutation", async () => {
    const calls = installFetch({
      [devicesUrl]: { json: { devices: [deviceOnline] } },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [renameUrl]: { status: 204 },
    });

    renderAt("/devices", <Devices />);
    await screen.findByText("Laptop A");

    await userEvent.click(screen.getByRole("button", { name: "Rename" }));
    const input = screen.getByLabelText("Device name");
    await userEvent.clear(input);
    await userEvent.type(input, "Primary Laptop");
    await userEvent.click(screen.getByRole("button", { name: "Save name" }));

    await waitFor(() => {
      const rename = calls.find((call) => call.path === "/api/v1/devices/device-1/rename");
      expect(rename).toBeTruthy();
      expect(rename?.headers["x-csrf-token"]).toBe("csrf-token-1");
      expect(JSON.parse(rename?.body ?? "{}")).toEqual({ name: "Primary Laptop" });
    });
  });

  it("warns that revoking keeps the license slot used, then revokes with CSRF", async () => {
    const calls = installFetch({
      [devicesUrl]: { json: { devices: [deviceOnline] } },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [revokeUrl]: { status: 204 },
    });

    renderAt("/devices", <Devices />);
    await screen.findByText("Laptop A");

    await userEvent.click(screen.getByRole("button", { name: "Revoke device" }));

    const dialog = screen.getByRole("dialog", { name: "Revoke this device?" });
    expect(dialog).toBeTruthy();
    const warning = screen.getByTestId("revoke-slot-warning");
    expect(warning.textContent).toMatch(/slot .*stays used|does not free/i);

    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(calls.filter((call) => call.method === "POST")).toHaveLength(0);

    await userEvent.click(screen.getByRole("button", { name: "Revoke device" }));
    await userEvent.click(screen.getByRole("button", { name: "Yes, revoke this device" }));

    await waitFor(() => {
      const revoke = calls.find((call) => call.path === "/api/v1/devices/device-1/revoke");
      expect(revoke).toBeTruthy();
      expect(revoke?.headers["x-csrf-token"]).toBe("csrf-token-1");
      expect(revoke?.body).toBeUndefined();
    });
  });

  it("rotates a credential through CSRF and keeps the returned secret in memory only", async () => {
    const localStorage = { getItem: vi.fn(), setItem: vi.fn() };
    const sessionStorage = { getItem: vi.fn(), setItem: vi.fn() };
    vi.stubGlobal("localStorage", localStorage);
    vi.stubGlobal("sessionStorage", sessionStorage);
    const calls = installFetch({
      [devicesUrl]: { json: { devices: [deviceOnline] } },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [rotateCredentialUrl]: {
        json: { device_id: "device-1", device_credential: "rkd_rotated_once_123456" },
      },
    });

    const view = renderAt("/devices", <Devices />);
    await screen.findByText("Laptop A");
    await userEvent.click(screen.getByRole("button", { name: "Rotate credential" }));
    const dialog = screen.getByRole("dialog", { name: "Rotate this credential?" });
    expect(dialog.textContent).toMatch(/invalidates the current credential/i);
    expect(dialog.textContent).toMatch(/disconnect/i);
    await userEvent.click(screen.getByRole("button", { name: "Yes, rotate credential" }));

    const credential = await screen.findByTestId("rotated-credential");
    expect(credential.textContent).toBe("rkd_rotated_once_123456");
    expect(screen.getByTestId("credential-once-warning").textContent).toMatch(
      /copy and store.*now.*not be shown again/i,
    );
    const rotation = calls.find(
      (call) => call.path === "/api/v1/devices/device-1/rotate-credential",
    );
    expect(rotation?.headers["x-csrf-token"]).toBe("csrf-token-1");
    expect(rotation?.body).toBeUndefined();
    expect(localStorage.getItem).not.toHaveBeenCalled();
    expect(localStorage.setItem).not.toHaveBeenCalled();
    expect(sessionStorage.getItem).not.toHaveBeenCalled();
    expect(sessionStorage.setItem).not.toHaveBeenCalled();

    view.unmount();
    renderAt("/devices", <Devices />);
    await screen.findByText("Laptop A");
    expect(screen.queryByTestId("rotated-credential")).toBeNull();
  });

  it("shows a recoverable error when credential rotation fails", async () => {
    installFetch({
      [devicesUrl]: { json: { devices: [deviceOnline] } },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [rotateCredentialUrl]: { status: 500 },
    });

    renderAt("/devices", <Devices />);
    await screen.findByText("Laptop A");
    await userEvent.click(screen.getByRole("button", { name: "Rotate credential" }));
    await userEvent.click(screen.getByRole("button", { name: "Yes, rotate credential" }));

    expect((await screen.findByRole("alert")).textContent).toMatch(
      /Rotating the credential failed.*existing credential is still active.*try again/i,
    );
    expect(screen.queryByTestId("rotated-credential")).toBeNull();
  });

  it("never renders token or credential values in the DOM", async () => {
    installFetch({
      [devicesUrl]: { json: { devices: [deviceOnline] } },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [revokeUrl]: { status: 204 },
    });

    renderAt("/devices", <Devices />);
    await screen.findByText("Laptop A");

    expect(document.body.textContent ?? "").not.toMatch(
      /rkd_|rkuc_|mca_|mcr_|mcp_[A-Za-z0-9]|Bearer |csrf-token/i,
    );
  });

  it("refreshes on a 10-second timer when visible and updates state", async () => {
    vi.useFakeTimers();
    const calls = installFetch({
      [devicesUrl]: [
        { json: { devices: [deviceOnline] } },
        { json: { devices: [deviceOnline, deviceOffline] } },
      ],
    });

    renderAt("/devices", <Devices />);
    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByText("Laptop A")).toBeTruthy();
    expect(screen.queryByText("Desktop B")).toBeNull();
    expect(calls.filter((c) => c.path === "/api/v1/devices")).toHaveLength(1);

    await act(async () => {
      vi.advanceTimersByTime(10000);
      await Promise.resolve();
    });

    expect(calls.filter((c) => c.path === "/api/v1/devices")).toHaveLength(2);
    expect(screen.getByText("Desktop B")).toBeTruthy();
  });

  it("pauses 10s timer when tab is hidden and refreshes immediately when resumed", async () => {
    vi.useFakeTimers();
    let visibilityState: DocumentVisibilityState = "visible";
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => visibilityState,
    });

    const calls = installFetch({
      [devicesUrl]: [
        { json: { devices: [deviceOnline] } },
        { json: { devices: [deviceOnline, deviceOffline] } },
      ],
    });

    renderAt("/devices", <Devices />);
    await act(async () => {
      await Promise.resolve();
    });
    expect(calls.filter((c) => c.path === "/api/v1/devices")).toHaveLength(1);

    // Hide the document
    visibilityState = "hidden";
    fireEvent(document, new Event("visibilitychange"));

    await act(async () => {
      vi.advanceTimersByTime(25000);
      await Promise.resolve();
    });

    // No new fetch while hidden
    expect(calls.filter((c) => c.path === "/api/v1/devices")).toHaveLength(1);

    // Show the document again
    visibilityState = "visible";
    fireEvent(document, new Event("visibilitychange"));
    await act(async () => {
      await Promise.resolve();
    });

    expect(calls.filter((c) => c.path === "/api/v1/devices")).toHaveLength(2);
    expect(screen.getByText("Desktop B")).toBeTruthy();
  });

  it("avoids overlapping concurrent fetches during slow responses", async () => {
    vi.useFakeTimers();
    const { promise: slowFirstPromise, resolve: resolveFirstFetch } =
      createDeferred<boolean>();

    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const raw = typeof input === "string" ? input : input.toString();
        if (raw.includes("/api/v1/devices")) {
          await slowFirstPromise;
          return {
            status: 200,
            ok: true,
            json: async () => ({ devices: [deviceOnline] }),
          } as unknown as Response;
        }
        return { status: 404, ok: false, json: async () => null } as unknown as Response;
      }),
    );

    renderAt("/devices", <Devices />);

    // Fast-forward timer while first fetch is still in flight
    await act(async () => {
      vi.advanceTimersByTime(15000);
      await Promise.resolve();
    });

    // Click manual refresh while first fetch is still unresolved
    const refreshBtn = screen.getByRole("button", { name: /refresh/i });
    fireEvent.click(refreshBtn);

    // Only 1 fetch call initiated because in-flight guard blocked overlaps
    expect(vi.mocked(fetch)).toHaveBeenCalledTimes(1);

    await act(async () => {
      resolveFirstFetch(true);
      await Promise.resolve();
    });

    expect(screen.getByText("Laptop A")).toBeTruthy();
  });

  it("supports manual refresh with busy state", async () => {
    const calls = installFetch({
      [devicesUrl]: [
        { json: { devices: [deviceOnline] } },
        { json: { devices: [deviceOnline, deviceOffline] } },
      ],
    });

    renderAt("/devices", <Devices />);
    expect(await screen.findByText("Laptop A")).toBeTruthy();
    expect(screen.queryByText("Desktop B")).toBeNull();

    const refreshBtn = screen.getByRole("button", { name: "Refresh" });
    await userEvent.click(refreshBtn);

    expect(await screen.findByText("Desktop B")).toBeTruthy();
    expect(calls.filter((c) => c.path === "/api/v1/devices")).toHaveLength(2);
  });

  it("preserves last good data and shows non-destructive retry banner on background refresh error", async () => {
    const calls = installFetch({
      [devicesUrl]: [
        { json: { devices: [deviceOnline] } },
        { status: 500 },
      ],
    });

    renderAt("/devices", <Devices />);
    expect(await screen.findByText("Laptop A")).toBeTruthy();

    const refreshBtn = screen.getByRole("button", { name: "Refresh" });
    await userEvent.click(refreshBtn);

    // Last good data remains displayed
    expect(screen.getByText("Laptop A")).toBeTruthy();
    expect(
      screen.getByText(/Could not refresh devices\. Showing last known state\./i),
    ).toBeTruthy();
    expect(calls.filter((c) => c.path === "/api/v1/devices")).toHaveLength(2);
  });

  it("stops timer and visibility listeners on unmount", async () => {
    vi.useFakeTimers();
    const calls = installFetch({
      [devicesUrl]: { json: { devices: [deviceOnline] } },
    });

    const view = renderAt("/devices", <Devices />);
    await act(async () => {
      await Promise.resolve();
    });
    expect(calls.filter((c) => c.path === "/api/v1/devices")).toHaveLength(1);

    view.unmount();

    await act(async () => {
      vi.advanceTimersByTime(20000);
      await Promise.resolve();
    });

    fireEvent(document, new Event("visibilitychange"));
    await act(async () => {
      await Promise.resolve();
    });

    expect(calls.filter((c) => c.path === "/api/v1/devices")).toHaveLength(1);
  });

  it("redirects to login and stops refresh when 401 unauthorized is received", async () => {
    vi.useFakeTimers();
    installFetch({
      [devicesUrl]: { status: 401, json: { error: "unauthorized" } },
    });

    renderAt("/devices", <Devices />);
    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByTestId("login-page")).toBeTruthy();
    expect(screen.queryByTestId("page-devices")).toBeNull();
  });

  it("coalesces and executes follow-up fetch when mutation occurs during in-flight read", async () => {
    vi.useFakeTimers();
    const { promise: backgroundFetchPromise, resolve: resolveBackgroundFetch } =
      createDeferred<void>();

    let deviceGetCount = 0;

    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const raw = typeof input === "string" ? input : input.toString();
        const method = (init?.method ?? "GET").toUpperCase();

        if (raw.includes("/api/v1/devices") && method === "GET") {
          deviceGetCount += 1;
          if (deviceGetCount === 1) {
            return {
              status: 200,
              ok: true,
              json: async () => ({ devices: [deviceOnline] }),
            } as unknown as Response;
          }
          if (deviceGetCount === 2) {
            // Background poll in-flight (still showing pre-mutation state)
            await backgroundFetchPromise;
            return {
              status: 200,
              ok: true,
              json: async () => ({ devices: [deviceOnline] }),
            } as unknown as Response;
          }
          // Follow-up fetch after mutation settles
          return {
            status: 200,
            ok: true,
            json: async () => ({ devices: [{ ...deviceOnline, name: "Renamed In Flight" }] }),
          } as unknown as Response;
        }

        if (raw.includes("/api/v1/csrf")) {
          return {
            status: 200,
            ok: true,
            json: async () => ({ csrf_token: "csrf-token-1" }),
          } as unknown as Response;
        }

        if (raw.includes("/rename") && method === "POST") {
          return {
            status: 204,
            ok: true,
            json: async () => null,
          } as unknown as Response;
        }

        return { status: 404, ok: false, json: async () => null } as unknown as Response;
      }),
    );

    renderAt("/devices", <Devices />);
    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByText("Laptop A")).toBeTruthy();
    expect(deviceGetCount).toBe(1);

    // Advance timer to trigger background fetch (deviceGetCount becomes 2 and holds)
    await act(async () => {
      vi.advanceTimersByTime(10000);
      await Promise.resolve();
    });
    expect(deviceGetCount).toBe(2);

    // Perform rename mutation while background fetch is still in-flight
    const renameBtn = screen.getByRole("button", { name: "Rename" });
    fireEvent.click(renameBtn);
    const input = screen.getByLabelText("Device name");
    fireEvent.change(input, { target: { value: "Renamed In Flight" } });
    const saveBtn = screen.getByRole("button", { name: "Save name" });
    fireEvent.click(saveBtn);

    // Let mutation complete (POST /rename returns 204 and calls fetchDevices())
    await act(async () => {
      await Promise.resolve();
    });

    // While background fetch is unresolved, no third GET has started yet
    expect(deviceGetCount).toBe(2);

    // Release the stale background fetch
    await act(async () => {
      resolveBackgroundFetch();
      await Promise.resolve();
    });

    // The queued refresh should have executed exactly once
    expect(deviceGetCount).toBe(3);
    expect(screen.getByText("Renamed In Flight")).toBeTruthy();
  });
});

describe("studios screen", () => {
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
    vi.unstubAllGlobals();
  });

  it("lists Studio sessions with their lifecycle state as text", async () => {
    installFetch({ [studiosUrl]: { json: { studios: [studioActive, studioEnded] } } });

    renderAt("/studios", <Studios />);

    expect(await screen.findByTestId("page-studios")).toBeTruthy();
    await screen.findByText("studio-alpha");
    expect(screen.getByText("studio-beta")).toBeTruthy();
    expect(screen.getByText("Active")).toBeTruthy();
    expect(screen.getByText("Ended")).toBeTruthy();
    expect(screen.getByText(/2026-09-03/)).toBeTruthy();
  });

  it("explains that no Studio has connected yet", async () => {
    installFetch({ [studiosUrl]: { json: { studios: [] } } });

    renderAt("/studios", <Studios />);

    expect(await screen.findByText(/no studio sessions/i)).toBeTruthy();
  });

  it("refreshes studios on 10s timer when visible and updates sessions", async () => {
    vi.useFakeTimers();
    const calls = installFetch({
      [studiosUrl]: [
        { json: { studios: [studioActive] } },
        { json: { studios: [studioActive, studioEnded] } },
      ],
    });

    renderAt("/studios", <Studios />);
    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByText("studio-alpha")).toBeTruthy();
    expect(screen.queryByText("studio-beta")).toBeNull();
    expect(calls.filter((c) => c.path === "/api/v1/studios")).toHaveLength(1);

    await act(async () => {
      vi.advanceTimersByTime(10000);
      await Promise.resolve();
    });

    expect(calls.filter((c) => c.path === "/api/v1/studios")).toHaveLength(2);
    expect(screen.getByText("studio-beta")).toBeTruthy();
  });

  it("pauses studios refresh when hidden and resumes on visible", async () => {
    vi.useFakeTimers();
    let visibilityState: DocumentVisibilityState = "visible";
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => visibilityState,
    });

    const calls = installFetch({
      [studiosUrl]: [
        { json: { studios: [studioActive] } },
        { json: { studios: [studioActive, studioEnded] } },
      ],
    });

    renderAt("/studios", <Studios />);
    await act(async () => {
      await Promise.resolve();
    });
    expect(calls.filter((c) => c.path === "/api/v1/studios")).toHaveLength(1);

    visibilityState = "hidden";
    fireEvent(document, new Event("visibilitychange"));

    await act(async () => {
      vi.advanceTimersByTime(25000);
      await Promise.resolve();
    });
    expect(calls.filter((c) => c.path === "/api/v1/studios")).toHaveLength(1);

    visibilityState = "visible";
    fireEvent(document, new Event("visibilitychange"));
    await act(async () => {
      await Promise.resolve();
    });

    expect(calls.filter((c) => c.path === "/api/v1/studios")).toHaveLength(2);
    expect(screen.getByText("studio-beta")).toBeTruthy();
  });

  it("avoids overlapping concurrent studio fetches", async () => {
    vi.useFakeTimers();
    const { promise: slowFirstPromise, resolve: resolveFirstFetch } =
      createDeferred<boolean>();

    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const raw = typeof input === "string" ? input : input.toString();
        if (raw.includes("/api/v1/studios")) {
          await slowFirstPromise;
          return {
            status: 200,
            ok: true,
            json: async () => ({ studios: [studioActive] }),
          } as unknown as Response;
        }
        return { status: 404, ok: false, json: async () => null } as unknown as Response;
      }),
    );

    renderAt("/studios", <Studios />);

    await act(async () => {
      vi.advanceTimersByTime(15000);
      await Promise.resolve();
    });

    const refreshBtn = screen.getByRole("button", { name: /refresh/i });
    fireEvent.click(refreshBtn);

    expect(vi.mocked(fetch)).toHaveBeenCalledTimes(1);

    await act(async () => {
      resolveFirstFetch(true);
      await Promise.resolve();
    });

    expect(screen.getByText("studio-alpha")).toBeTruthy();
  });

  it("supports manual studios refresh with busy state", async () => {
    const calls = installFetch({
      [studiosUrl]: [
        { json: { studios: [studioActive] } },
        { json: { studios: [studioActive, studioEnded] } },
      ],
    });

    renderAt("/studios", <Studios />);
    expect(await screen.findByText("studio-alpha")).toBeTruthy();
    expect(screen.queryByText("studio-beta")).toBeNull();

    const refreshBtn = screen.getByRole("button", { name: "Refresh" });
    await userEvent.click(refreshBtn);

    expect(await screen.findByText("studio-beta")).toBeTruthy();
    expect(calls.filter((c) => c.path === "/api/v1/studios")).toHaveLength(2);
  });

  it("preserves last good studios data and shows non-destructive retry banner on error", async () => {
    const calls = installFetch({
      [studiosUrl]: [
        { json: { studios: [studioActive] } },
        { status: 500 },
      ],
    });

    renderAt("/studios", <Studios />);
    expect(await screen.findByText("studio-alpha")).toBeTruthy();

    const refreshBtn = screen.getByRole("button", { name: "Refresh" });
    await userEvent.click(refreshBtn);

    expect(screen.getByText("studio-alpha")).toBeTruthy();
    expect(
      screen.getByText(/Could not refresh Studio sessions\. Showing last known state\./i),
    ).toBeTruthy();
    expect(calls.filter((c) => c.path === "/api/v1/studios")).toHaveLength(2);
  });

  it("cleans up studios timer and visibility listener on unmount", async () => {
    vi.useFakeTimers();
    const calls = installFetch({
      [studiosUrl]: { json: { studios: [studioActive] } },
    });

    const view = renderAt("/studios", <Studios />);
    await act(async () => {
      await Promise.resolve();
    });
    expect(calls.filter((c) => c.path === "/api/v1/studios")).toHaveLength(1);

    view.unmount();

    await act(async () => {
      vi.advanceTimersByTime(20000);
      await Promise.resolve();
    });

    fireEvent(document, new Event("visibilitychange"));
    await act(async () => {
      await Promise.resolve();
    });

    expect(calls.filter((c) => c.path === "/api/v1/studios")).toHaveLength(1);
  });

  it("redirects to login when 401 unauthorized is received on studios", async () => {
    vi.useFakeTimers();
    installFetch({
      [studiosUrl]: { status: 401, json: { error: "unauthorized" } },
    });

    renderAt("/studios", <Studios />);
    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByTestId("login-page")).toBeTruthy();
    expect(screen.queryByTestId("page-studios")).toBeNull();
  });
});

describe("diagnostics screen", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("renders the service summary counts exactly as the API reports them", async () => {
    installFetch({ [diagnosticsUrl]: { json: diagnostics } });

    renderAt("/diagnostics", <Diagnostics />);

    expect(await screen.findByTestId("page-diagnostics")).toBeTruthy();
    expect(screen.getByTestId("diagnostics-database").textContent).toMatch(/ok/i);
    expect(screen.getByTestId("diagnostics-devices-registered").textContent).toBe("2");
    expect(screen.getByTestId("diagnostics-devices-online").textContent).toBe("1");
    expect(screen.getByTestId("diagnostics-studios-active").textContent).toBe("1");
  });

  it("renders sanitized per-device operational diagnostics and unavailable states", async () => {
    installFetch({ [diagnosticsUrl]: { json: diagnostics } });

    renderAt("/diagnostics", <Diagnostics />);

    const online = await screen.findByTestId("diagnostic-device-device-1");
    expect(online.textContent).toMatch(/Laptop A/);
    expect(online.textContent).toMatch(/ready/i);
    expect(online.textContent).toContain("2026-09-04");
    expect(online.textContent).toContain("3");
    expect(online.textContent).toContain("Connection reset; retry scheduled");

    const offline = screen.getByTestId("diagnostic-device-device-2");
    expect(offline.textContent).toMatch(/Desktop B/);
    expect(offline.textContent).toMatch(/Unavailable/);
    expect(offline.textContent).toMatch(/None reported/);
  });

  it("keeps the page test id when the diagnostics API fails", async () => {
    installFetch({ [diagnosticsUrl]: { status: 500 } });

    renderAt("/diagnostics", <Diagnostics />);

    expect(await screen.findByText(/Diagnostics unavailable/i)).toBeTruthy();
    expect(screen.getByTestId("page-diagnostics").textContent).toMatch(/Diagnostics/i);
  });
});

describe("status badge accessibility", () => {
  afterEach(() => {
    cleanup();
  });

  it("conveys every status as text plus a distinct shape, never color alone", () => {
    render(<StatusBadge status="online" />);
    render(<StatusBadge status="offline" />);
    render(<StatusBadge status="revoked" />);

    expect(screen.getAllByText("Online")).toHaveLength(1);
    expect(screen.getAllByText("Offline")).toHaveLength(1);
    expect(screen.getAllByText("Revoked")).toHaveLength(1);

    const shapes = screen
      .getAllByTestId("status-shape")
      .map((element) => element.textContent);
    expect(new Set(shapes).size).toBe(3);
  });

  it("falls back to the raw status word for unknown statuses", () => {
    render(<StatusBadge status="degraded" />);
    expect(screen.getByText("degraded")).toBeTruthy();
  });
});

describe("confirm dialog keyboard interaction", () => {
  afterEach(() => {
    cleanup();
  });

  it("starts on Cancel, reaches confirm with Tab, confirms with Enter", async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        title="Revoke this device?"
        body={<p>Are you sure?</p>}
        confirmLabel="Yes, revoke this device"
        onConfirm={onConfirm}
        onCancel={onCancel}
      />,
    );

    const cancel = screen.getByRole("button", { name: "Cancel" });
    const confirm = screen.getByRole("button", { name: "Yes, revoke this device" });
    expect(document.activeElement).toBe(cancel);

    await userEvent.tab();
    expect(document.activeElement).toBe(confirm);

    await userEvent.keyboard("{Enter}");
    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(onCancel).not.toHaveBeenCalled();
  });

  it("cancels on Escape and keeps Tab focus trapped inside the dialog", async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        title="Revoke this device?"
        body={<p>Are you sure?</p>}
        confirmLabel="Yes, revoke this device"
        onConfirm={onConfirm}
        onCancel={onCancel}
      />,
    );

    const cancel = screen.getByRole("button", { name: "Cancel" });
    const confirm = screen.getByRole("button", { name: "Yes, revoke this device" });

    await userEvent.tab({ shift: true });
    expect(document.activeElement).toBe(confirm);

    await userEvent.tab();
    expect(document.activeElement).toBe(cancel);

    await userEvent.keyboard("{Escape}");
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onConfirm).not.toHaveBeenCalled();
  });
});
