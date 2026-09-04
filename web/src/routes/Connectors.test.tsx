import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";

import Connectors from "./Connectors";

type MockRoute = { status?: number; json?: unknown };

type RecordedCall = {
  method: string;
  path: string;
  headers: Record<string, string>;
  body?: string;
};

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
        <Route path="*" element={element} />
      </Routes>
    </MemoryRouter>,
  );
}

const connectorsUrl = "GET /api/v1/connectors";
const devicesUrl = "GET /api/v1/devices";
const studiosUrl = "GET /api/v1/studios";
const csrfUrl = "GET /api/v1/csrf";
const targetUrl = "POST /api/v1/connectors/grant-1/target";
const revokeUrl = "POST /api/v1/connectors/grant-1/revoke";

const device1 = {
  id: "device-1",
  name: "Laptop A",
  status: "active",
  online: true,
  created_at: "2026-09-01T10:00:00Z",
  updated_at: "2026-09-04T10:00:00Z",
};
const device2 = {
  id: "device-2",
  name: "Desktop B",
  status: "active",
  online: false,
  created_at: "2026-09-02T10:00:00Z",
  updated_at: "2026-09-03T10:00:00Z",
};
const studioAlpha = {
  id: "studio-1",
  device_id: "device-1",
  studio_id: "studio-alpha",
  status: "active",
  started_at: "2026-09-04T09:00:00Z",
  ended_at: null,
};
const studioGamma = {
  id: "studio-3",
  device_id: "device-1",
  studio_id: "studio-gamma",
  status: "active",
  started_at: "2026-09-04T09:30:00Z",
  ended_at: null,
};
const studioBetaEnded = {
  id: "studio-2",
  device_id: "device-1",
  studio_id: "studio-beta",
  status: "ended",
  started_at: "2026-09-03T09:00:00Z",
  ended_at: "2026-09-03T18:00:00Z",
};
const chatgptConnector = {
  id: "grant-1",
  client_id: "https://chatgpt.com/aip/mcp",
  client_name: "ChatGPT",
  scopes: ["studio:read", "studio:edit"],
  resource: "https://gateway.example.test/mcp",
  device_id: "device-1",
  studio_session_id: null,
  created_at: "2026-09-04T08:00:00Z",
  revoked_at: null,
};

describe("connectors screen", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("shows each connector's client, scopes, and explicit target", async () => {
    installFetch({
      [connectorsUrl]: {
        json: {
          connectors: [{ ...chatgptConnector, studio_session_id: "studio-1" }],
        },
      },
      [devicesUrl]: { json: { devices: [device1, device2] } },
      [studiosUrl]: { json: { studios: [studioAlpha] } },
    });

    renderAt("/connectors", <Connectors />);

    expect(await screen.findByTestId("page-connectors")).toBeTruthy();
    await screen.findByText("ChatGPT");
    expect(screen.getByText("https://chatgpt.com/aip/mcp")).toBeTruthy();
    const scopes = screen.getByTestId("connector-scopes-grant-1");
    expect(scopes.textContent).toContain("studio:read");
    expect(scopes.textContent).toContain("studio:edit");
    const target = screen.getByTestId("connector-target-grant-1");
    expect(target.textContent).toContain("Laptop A");
    expect(target.textContent).toContain("studio-alpha");
  });

  it("blocks an ambiguous target: two active Studios, none chosen", async () => {
    installFetch({
      [connectorsUrl]: { json: { connectors: [chatgptConnector] } },
      [devicesUrl]: { json: { devices: [device1] } },
      [studiosUrl]: { json: { studios: [studioAlpha, studioGamma, studioBetaEnded] } },
    });

    renderAt("/connectors", <Connectors />);

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/blocked/i);
    expect(alert.textContent).toMatch(/2 Studio sessions/i);
    expect(alert.textContent).toMatch(/no Studio is chosen/i);
  });

  it("resolves the ambiguity by saving an explicit Studio target with CSRF", async () => {
    const calls = installFetch({
      [connectorsUrl]: [
        { json: { connectors: [chatgptConnector] } },
        {
          json: {
            connectors: [{ ...chatgptConnector, studio_session_id: "studio-1" }],
          },
        },
      ],
      [devicesUrl]: { json: { devices: [device1, device2] } },
      [studiosUrl]: { json: { studios: [studioAlpha, studioGamma] } },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [targetUrl]: { status: 204 },
    });

    renderAt("/connectors", <Connectors />);
    await screen.findByRole("alert");

    const studioSelect = screen.getByLabelText("Studio session");
    await userEvent.selectOptions(studioSelect, "studio-1");
    await userEvent.click(screen.getByRole("button", { name: "Save target" }));

    await waitFor(() => {
      const target = calls.find(
        (call) => call.method === "POST" && call.path === "/api/v1/connectors/grant-1/target",
      );
      expect(target).toBeTruthy();
      expect(target?.headers["x-csrf-token"]).toBe("csrf-token-1");
      expect(JSON.parse(target?.body ?? "{}")).toEqual({
        device_id: "device-1",
        studio_session_id: "studio-1",
      });
    });
    // The saved target replaces the ambiguity alert.
    await waitFor(() => {
      expect(screen.queryByRole("alert")).toBeNull();
    });
    expect(screen.getByTestId("connector-target-grant-1").textContent).toContain("studio-alpha");
  });

  it("revokes a connector only after an explicit confirmation", async () => {
    const calls = installFetch({
      [connectorsUrl]: [
        { json: { connectors: [chatgptConnector] } },
        {
          json: {
            connectors: [{ ...chatgptConnector, revoked_at: "2026-09-04T12:00:00Z" }],
          },
        },
      ],
      [devicesUrl]: { json: { devices: [device1] } },
      [studiosUrl]: { json: { studios: [studioAlpha] } },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [revokeUrl]: { status: 204 },
    });

    renderAt("/connectors", <Connectors />);
    await screen.findByText("ChatGPT");

    await userEvent.click(screen.getByRole("button", { name: "Revoke connector" }));
    const dialog = screen.getByRole("dialog", { name: "Revoke this connector?" });
    expect(dialog.textContent).toMatch(/ChatGPT/);

    await userEvent.click(screen.getByRole("button", { name: "Yes, revoke this connector" }));

    await waitFor(() => {
      const revoke = calls.find(
        (call) => call.method === "POST" && call.path === "/api/v1/connectors/grant-1/revoke",
      );
      expect(revoke).toBeTruthy();
      expect(revoke?.headers["x-csrf-token"]).toBe("csrf-token-1");
    });
    // The revoked grant keeps its row with a revoked state and no controls.
    await waitFor(() => {
      expect(screen.getByTestId("connector-grant-1").textContent).toMatch(/Revoked/);
    });
    expect(screen.queryByRole("button", { name: "Revoke connector" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save target" })).toBeNull();
  });

  it("explains that no connector has authorized yet", async () => {
    installFetch({
      [connectorsUrl]: { json: { connectors: [] } },
      [devicesUrl]: { json: { devices: [] } },
      [studiosUrl]: { json: { studios: [] } },
    });

    renderAt("/connectors", <Connectors />);

    expect(await screen.findByText(/no connectors/i)).toBeTruthy();
  });

  it("never renders token or credential values in the DOM", async () => {
    installFetch({
      [connectorsUrl]: { json: { connectors: [chatgptConnector] } },
      [devicesUrl]: { json: { devices: [device1] } },
      [studiosUrl]: { json: { studios: [studioAlpha] } },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [targetUrl]: { status: 204 },
    });

    renderAt("/connectors", <Connectors />);
    await screen.findByText("ChatGPT");

    expect(document.body.textContent ?? "").not.toMatch(
      /rkd_|rkuc_|mca_|mcr_|mcp_[A-Za-z0-9]|Bearer |csrf-token/i,
    );
  });
});
