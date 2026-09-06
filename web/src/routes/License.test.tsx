import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";

import License from "./License";

type MockRoute = { status?: number; json?: unknown; headers?: Record<string, string> };

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
        <Route path="/download" element={<div />} />
        <Route path="*" element={element} />
      </Routes>
    </MemoryRouter>,
  );
}

const licenseUrl = "GET /api/v1/license";
const serverDateHeader = { date: "Fri, 04 Sep 2026 11:00:00 GMT" };

const activeTrialAndLicense = {
  owner: {
    roblox_id_masked: "12•••••89",
    display_name: "BuilderRoblox",
  },
  trial: {
    active: true,
    started_at: "2026-09-04T11:00:00Z",
    ends_at: "2026-09-18T11:00:00Z",
  },
  license: {
    status: "active",
    expires_at: "2027-09-04T11:00:00Z",
    subscription_id: "sub_masked_42",
    device_slots: 3,
    active_bindings: 2,
    available_slots: 1,
    allowed_scopes: ["mcp:connect", "studio:read"],
    usage_limit: 10000,
    current_usage: 245,
    transfer_status: "not_requested",
    recovery_status: "reviewing",
  },
};
const expiredTrialNoLicense = {
  owner: {
    roblox_id_masked: "12•••••89",
    display_name: "BuilderRoblox",
  },
  trial: {
    active: false,
    started_at: "2026-08-01T11:00:00Z",
    ends_at: "2026-08-15T11:00:00Z",
  },
  license: null,
};
const nothingStarted = {
  owner: {
    roblox_id_masked: "12•••••89",
    display_name: "BuilderRoblox",
  },
  trial: null,
  license: null,
};

describe("license screen", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("shows the active trial window with a server-anchored remaining count", async () => {
    installFetch({ [licenseUrl]: { json: activeTrialAndLicense, headers: serverDateHeader } });

    renderAt("/license", <License />);

    expect(await screen.findByTestId("page-license")).toBeTruthy();
    expect(await screen.findByText(/Free trial active/i)).toBeTruthy();
    const window = screen.getByTestId("trial-window");
    expect(window.textContent).toContain("2026-09-04");
    expect(window.textContent).toContain("2026-09-18");
    expect(screen.getByTestId("trial-remaining").textContent).toMatch(/14 days remaining/);
  });

  it("shows the paid license slot usage", async () => {
    installFetch({ [licenseUrl]: { json: activeTrialAndLicense } });

    renderAt("/license", <License />);

    await screen.findByTestId("license-state");
    expect(screen.getByTestId("license-state").textContent).toMatch(/active/i);
    expect(screen.getByTestId("license-state").textContent).toContain("2");
    expect(screen.getByTestId("license-state").textContent).toContain("1");
  });

  it("renders owner, subscription, slots, scopes, usage, transfer, and recovery details", async () => {
    installFetch({ [licenseUrl]: { json: activeTrialAndLicense } });

    renderAt("/license", <License />);

    const owner = await screen.findByTestId("license-owner");
    expect(owner.textContent).toContain("BuilderRoblox");
    expect(owner.textContent).toContain("12•••••89");
    expect(screen.getByTestId("license-expiry").textContent).toContain("2027-09-04");
    expect(screen.getByTestId("license-subscription").textContent).toBe("sub_masked_42");
    expect(screen.getByTestId("license-slots").textContent).toMatch(/3.*2.*1/);
    expect(screen.getByTestId("license-scopes").textContent).toMatch(
      /mcp:connect.*studio:read/,
    );
    expect(screen.getByTestId("license-usage").textContent).toMatch(/245.*10,000/);
    expect(screen.getByTestId("license-transfer-status").textContent).toMatch(
      /not requested/i,
    );
    expect(screen.getByTestId("license-recovery-status").textContent).toMatch(/reviewing/i);
  });

  it("renders explicit unavailable states for nullable paid-license details", async () => {
    installFetch({
      [licenseUrl]: {
        json: {
          ...activeTrialAndLicense,
          license: {
            ...activeTrialAndLicense.license,
            expires_at: null,
            subscription_id: null,
            allowed_scopes: [],
            usage_limit: null,
            transfer_status: null,
            recovery_status: null,
          },
        },
      },
    });

    renderAt("/license", <License />);

    await screen.findByTestId("license-state");
    expect(screen.getByTestId("license-expiry").textContent).toBe("Unavailable");
    expect(screen.getByTestId("license-subscription").textContent).toBe("Unavailable");
    expect(screen.getByTestId("license-scopes").textContent).toBe("None allowed");
    expect(screen.getByTestId("license-usage").textContent).toMatch(/245.*Unlimited/i);
    expect(screen.getByTestId("license-transfer-status").textContent).toBe("Unavailable");
    expect(screen.getByTestId("license-recovery-status").textContent).toBe("Unavailable");
  });

  it("offers an upgrade call to action when the trial has expired", async () => {
    installFetch({ [licenseUrl]: { json: expiredTrialNoLicense } });

    renderAt("/license", <License />);

    const cta = await screen.findByTestId("upgrade-cta");
    expect(cta.textContent).toMatch(/free trial has ended/i);
    expect(cta.textContent).toMatch(/purchase a license/i);
  });

  it("explains that the trial starts only at first PC connection", async () => {
    installFetch({ [licenseUrl]: { json: nothingStarted } });

    renderAt("/license", <License />);

    const notice = await screen.findByText(/No free trial yet/i);
    expect(notice.tagName).toBe("H3");
    expect(screen.getByText(/starts only when your first PC/i)).toBeTruthy();
  });

  it("offers no self-unbind, self-rebind, or self-transfer control of any kind", async () => {
    const calls = installFetch({ [licenseUrl]: { json: activeTrialAndLicense } });

    renderAt("/license", <License />);
    await screen.findByTestId("license-state");

    // No control may unbind a device from a license slot, rebind the Roblox
    // identity, or transfer the license: those are admin-only operations.
    for (const pattern of [/unbind/i, /rebind/i, /transfer/i, /release/i, /detach/i]) {
      const buttons = screen.queryAllByRole("button", { name: pattern });
      const links = screen.queryAllByRole("link", { name: pattern });
      expect([...buttons, ...links]).toHaveLength(0);
    }
    // The license screen is read-only: it must never issue a mutation.
    expect(calls.filter((call) => call.method !== "GET")).toHaveLength(0);
    expect(calls.every((call) => call.path === "/api/v1/license")).toBe(true);
  });

  it("never renders token or credential values in the DOM", async () => {
    installFetch({ [licenseUrl]: { json: activeTrialAndLicense } });

    renderAt("/license", <License />);
    await screen.findByTestId("license-state");

    expect(document.body.textContent ?? "").not.toMatch(
      /rkd_|rkuc_|mca_|mcr_|mcp_[A-Za-z0-9]|Bearer |csrf-/i,
    );
  });
});
