import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";

import Download from "./Download";
import Login from "./Login";

type MockRoute = { status?: number; json?: unknown; headers?: Record<string, string> };

type RecordedCall = {
  method: string;
  path: string;
  headers: Record<string, string>;
  body?: string;
};

// installFetch stubs window.fetch with per-route responses keyed by
// "METHOD /path" (query strings ignored) and records every call.
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
      // status, ok, json(), and (for the server clock) headers contract.
      return {
        status,
        ok: status >= 200 && status < 300,
        headers: {
          get(name: string) {
            return route?.headers?.[name.toLowerCase()] ?? null;
          },
        },
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
        <Route path="*" element={element} />
      </Routes>
    </MemoryRouter>,
  );
}

const meUrl = "GET /api/v1/me";
const metadataUrl = "GET /api/v1/bridge/download/metadata";

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
// The Go server always stamps responses with its own clock; the client must
// anchor the countdown to this instant, never to the browser's clock.
const serverDateHeader = { date: "Fri, 04 Sep 2026 11:00:00 GMT" };

describe("download screen", () => {
  beforeEach(() => {
    vi.useRealTimers();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("displays checksum, version, size, and the download link", async () => {
    installFetch({
      [meUrl]: { json: freshMe, headers: serverDateHeader },
      [metadataUrl]: { json: metadata },
    });

    renderAt("/download", <Download />);

    await screen.findByText("Signed in as Builder 1516563360");
    expect(screen.getByTestId("bridge-version").textContent).toBe("1.4.2");
    expect(screen.getByTestId("bridge-checksum").textContent).toBe("a".repeat(64));
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

  it("computes the trial countdown from the server clock, not the client clock", async () => {
    installFetch({
      [meUrl]: { json: activeTrialMe, headers: serverDateHeader },
      [metadataUrl]: { json: metadata },
    });
    // The browser clock is a full day ahead of the server. A countdown that
    // trusted the client clock would report 13 days; only a server-anchored
    // countdown reports the true 14 remaining days.
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-09-05T11:00:00Z"));

    renderAt("/download", <Download />);

    const countdown = await screen.findByTestId("trial-countdown");
    expect(countdown.textContent).toMatch(/14 days remaining/);
    // The window itself is rendered from the server-provided timestamps.
    const state = screen.getByTestId("trial-state");
    expect(state.textContent).toMatch(/2026-09-18/);
  });

  it("never renders token or credential values in the DOM", async () => {
    installFetch({
      [meUrl]: { json: activeTrialMe, headers: serverDateHeader },
      [metadataUrl]: { json: metadata },
    });

    renderAt("/download", <Download />);

    await screen.findByTestId("bridge-checksum");
    expect(document.body.textContent ?? "").not.toMatch(
      /rkd_|rkuc_|mca_|mcr_|mcp_[A-Za-z0-9]|Bearer |csrf-/i,
    );
  });
});
