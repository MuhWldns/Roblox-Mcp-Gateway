import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
    vi.useRealTimers();
  });

  it("redirects unauthenticated visitors to the login page", async () => {
    installFetch({ [meUrl]: { status: 401 } });

    renderAt("/download", <Download />);

    expect((await screen.findByRole("link", { name: /continue with/i })).getAttribute("href")).toBe("/api/v1/auth/roblox/login?next=%2Fdownload");
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
    const approveButton = screen.getByRole("button", { name: /approve/i });
    expect(approveButton.hasAttribute("disabled")).toBe(false);
    await userEvent.click(approveButton);

    await waitFor(() => {
      expect(screen.getByTestId("approval-status").textContent).toMatch(/approved/i);
    });
    const approve = calls.find((call) => call.path === "/api/v1/enrollments/approve");
    expect(approve).toBeTruthy();
    expect(approve?.headers["x-csrf-token"]).toBe("csrf-token-1");
    expect(JSON.parse(approve?.body ?? "{}")).toEqual({ user_code: "rkuc_TEST123" });
  });

  it("shows the first-binding trial state once the device completes its exchange", async () => {
    vi.useFakeTimers();
    const calls = installFetch({
      [meUrl]: [{ json: freshMe }, { json: activeTrialMe }],
      [claimUrl]: [
        { json: { ...claim, status: "pending" } },
        { json: { ...claim, status: "pending" } },
        { status: 404 },
      ],
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [approveUrl]: { status: 204 },
    });

    renderAt("/enroll?code=rkuc_TEST123", <Enroll />);

    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByTestId("device-hostname")).toBeTruthy();
    expect(screen.getByTestId("device-hostname").textContent).toBe("DESKTOP-ABC123");

    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByTestId("approval-status")).toBeTruthy();
    expect(screen.getByText("Waiting for your computer to finish connecting…")).toBeTruthy();

    // During pending status, no extra /me requests should occur.
    // The initial mount made 1 /me request; claim polling should only poll /claim while status is pending.
    const meCallsInitial = calls.filter((c) => c.path === "/api/v1/me");
    expect(meCallsInitial.length).toBe(1);

    // Advance through poll ticks to 404 (tick 1 at 1500ms, tick 2 at 3000ms)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    const trial = screen.getByTestId("trial-state");
    expect(trial.textContent).toMatch(/free trial active/i);
    expect(trial.textContent).toMatch(/2026-09-18/);
    expect(screen.queryByText("Waiting for your computer to finish connecting…")).toBeNull();

    // After exchange completed (404 claim), exactly one additional /me call was made to verify trial.
    const meCallsFinal = calls.filter((c) => c.path === "/api/v1/me");
    expect(meCallsFinal.length).toBe(2);

    // Ensure polling stops once active trial is received
    const callCountAtCompletion = calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4500);
    });
    expect(calls.length).toBe(callCountAtCompletion);
  });

  it("transitions from approved to terminal denial when claim status becomes license_required and stops polling", async () => {
    vi.useFakeTimers();
    const calls = installFetch({
      [meUrl]: { json: freshMe },
      [claimUrl]: [
        { json: { ...claim, status: "pending" } },
        { json: { ...claim, status: "pending" } },
        { json: { ...claim, status: "license_required" } },
      ],
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [approveUrl]: { status: 204 },
    });

    renderAt("/enroll?code=rkuc_TEST123", <Enroll />);

    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByTestId("device-hostname")).toBeTruthy();
    expect(screen.getByTestId("device-hostname").textContent).toBe("DESKTOP-ABC123");

    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await act(async () => {
      await Promise.resolve();
    });

    // Initially approved state is rendered with indefinite waiting copy
    expect(screen.getByTestId("approval-status")).toBeTruthy();
    expect(screen.getByText("Computer approved")).toBeTruthy();
    expect(screen.getByText("Waiting for your computer to finish connecting…")).toBeTruthy();
    expect(screen.queryByTestId("license-required-status")).toBeNull();

    // Advance through poll ticks until license_required (tick 1 at 1500ms, tick 2 at 3000ms)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    // Once license_required is returned, terminal denial is rendered
    const denialSection = screen.getByTestId("license-required-status");
    const alert = screen.getByRole("alert");

    // Exact operator-facing denial copy
    expect(alert.textContent).toBe("You don’t have a license. Please contact support to get a license.");
    expect(within(denialSection).getByRole("link", { name: /back to setup/i }).getAttribute("href")).toBe("/setup");

    // The indefinite waiting copy and approval status disappear
    expect(screen.queryByTestId("approval-status")).toBeNull();
    expect(screen.queryByText("Computer approved")).toBeNull();
    expect(screen.queryByText("Waiting for your computer to finish connecting…")).toBeNull();

    // Ensure no technical language or internal details leaked
    const pageContent = denialSection.textContent ?? "";
    expect(pageContent).not.toMatch(/fingerprint|collision|trial[-_ ]reuse|status|code|403|409|json|api/i);
    // During pending status leading to license_required, no repeated /me calls were made
    const meCalls = calls.filter((c) => c.path === "/api/v1/me");
    expect(meCalls.length).toBe(1); // Only the initial mount /me call

    // Record call count after denial and ensure polling has stopped
    const callCountAtDenial = calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4500);
    });
    expect(calls.length).toBe(callCountAtDenial);
  });

  it("does not poll /me repeatedly while claim status remains pending", async () => {
    vi.useFakeTimers();
    const calls = installFetch({
      [meUrl]: { json: freshMe },
      [claimUrl]: { json: { ...claim, status: "pending" } },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [approveUrl]: { status: 204 },
    });

    renderAt("/enroll?code=rkuc_TEST123", <Enroll />);

    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByTestId("device-hostname")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByTestId("approval-status")).toBeTruthy();
    expect(screen.getByText("Waiting for your computer to finish connecting…")).toBeTruthy();

    // Advance timers for 2 polling ticks (3000ms)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    // Verify claim was polled but /me was not fetched again during pending state
    const meCalls = calls.filter((c) => c.path === "/api/v1/me");
    expect(meCalls.length).toBe(1);

    const claimCalls = calls.filter((c) => c.path === "/api/v1/enrollments/claim");
    // 1 initial load + 2 poll ticks
    expect(claimCalls.length).toBeGreaterThanOrEqual(2);
    // Assert bounded frequency: in 3s with a 1.5s interval, at most 4 claim calls
    expect(claimCalls.length).toBeLessThanOrEqual(5);
  });

  it("stops polling and displays actionable expired error on 410 without classifying active trial as pairing success", async () => {
    vi.useFakeTimers();
    const calls = installFetch({
      [meUrl]: [{ json: freshMe }, { json: activeTrialMe }],
      [claimUrl]: [
        { json: { ...claim, status: "pending" } },
        { status: 410 },
      ],
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [approveUrl]: { status: 204 },
    });

    renderAt("/enroll?code=rkuc_TEST123", <Enroll />);

    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByTestId("device-hostname")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByTestId("approval-status")).toBeTruthy();
    expect(screen.getByText("Waiting for your computer to finish connecting…")).toBeTruthy();

    // Advance timers for 1 polling tick (1500ms) to encounter 410
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });

    // When 410 is encountered, pairing-expired terminal state is shown
    const expiredSection = screen.getByTestId("pairing-expired-status");
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toBe("This pairing code has expired. Please start pairing again in Buildly Companion.");
    expect(within(expiredSection).getByRole("link", { name: /back to setup/i }).getAttribute("href")).toBe("/setup");

    // Crucial: Must never show connection success or trial-state active banner for this pairing
    expect(screen.queryByTestId("trial-state")).toBeNull();
    expect(screen.queryByTestId("approval-status")).toBeNull();
    expect(screen.queryByText("Waiting for your computer to finish connecting…")).toBeNull();

    // Polling must stop immediately and not query /me to falsely claim success
    const meCalls = calls.filter((c) => c.path === "/api/v1/me");
    expect(meCalls.length).toBe(1); // Only the initial mount /me call

    const callCountAtExpiration = calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4500);
    });
    expect(calls.length).toBe(callCountAtExpiration);
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
