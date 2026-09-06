import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RouterProvider, createMemoryRouter } from "react-router";

type MockRoute = { status?: number; json?: unknown };

type RecordedCall = {
  method: string;
  path: string;
  credentials?: RequestCredentials;
  headers: Record<string, string>;
  body?: string;
};

// installFetch stubs window.fetch with per-route responses keyed by
// "METHOD /path" (query strings ignored) and records every call, including
// the credentials mode the browser API client must always send.
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
        credentials: init?.credentials,
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

// renderShellAt mounts the real route tree in an in-memory data router so
// loaders, redirects, error boundaries, and nested navigation all execute
// exactly as they do under createBrowserRouter. The route modules are
// re-imported after vi.resetModules() so every test starts with a clean
// module-scoped CSRF cache, exactly like a freshly loaded browser page.
async function renderShellAt(path: string) {
  const { appRoutes } = await import("./router");
  const router = createMemoryRouter(appRoutes(), { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

const meUrl = "GET /api/v1/me";
const metadataUrl = "GET /api/v1/bridge/download/metadata";
const csrfUrl = "GET /api/v1/csrf";
const claimUrl = "GET /api/v1/enrollments/claim";
const approveUrl = "POST /api/v1/enrollments/approve";
const logoutUrl = "POST /api/v1/auth/logout";

const freshMe = { user_id: "u1", display_name: "Builder 1516563360", trial: null };
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

describe("dashboard shell routing", () => {
  beforeEach(() => {
    vi.resetModules();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("redirects unauthenticated visitors to the login page", async () => {
    installFetch({ [meUrl]: { status: 401 } });

    await renderShellAt("/devices");

    expect(await screen.findByText("Continue with Roblox")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Privacy Policy" }).getAttribute("href")).toBe("/privacy");
    expect(screen.getByRole("link", { name: "Terms of Service" }).getAttribute("href")).toBe("/terms");
  });

  it("renders the authenticated shell with section navigation", async () => {
    installFetch({ [meUrl]: { json: freshMe } });

    await renderShellAt("/devices");

    expect(await screen.findByText("Signed in as Builder 1516563360")).toBeTruthy();
    expect(screen.getByTestId("app-nav")).toBeTruthy();
    for (const label of ["Devices", "Studios", "Connectors", "License", "Diagnostics", "Admin"]) {
      expect(screen.getByRole("link", { name: label })).toBeTruthy();
    }
    expect(screen.getByTestId("page-devices").textContent).toMatch(/devices/i);
    expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Privacy Policy" }).getAttribute("href")).toBe("/privacy");
    expect(screen.getByRole("link", { name: "Terms of Service" }).getAttribute("href")).toBe("/terms");
  });

  it("navigates between dashboard sections without losing the shell", async () => {
    installFetch({ [meUrl]: { json: freshMe } });

    await renderShellAt("/devices");
    await screen.findByText("Signed in as Builder 1516563360");

    await userEvent.click(screen.getByRole("link", { name: "Studios" }));
    expect(await screen.findByTestId("page-studios")).toBeTruthy();
    expect(screen.getByText("Signed in as Builder 1516563360")).toBeTruthy();

    await userEvent.click(screen.getByRole("link", { name: "Admin" }));
    expect(await screen.findByTestId("page-admin")).toBeTruthy();
    expect(screen.getByTestId("app-nav")).toBeTruthy();
  });

  it("renders the error boundary instead of crashing when the session API fails", async () => {
    installFetch({ [meUrl]: [{ status: 500 }, { json: freshMe }] });

    await renderShellAt("/devices");

    expect(await screen.findByTestId("error-page")).toBeTruthy();
    expect(screen.getByText("Something went wrong")).toBeTruthy();

    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByTestId("page-devices")).toBeTruthy();
    expect(screen.getByText("Signed in as Builder 1516563360")).toBeTruthy();
  });

  it("keeps the enrollment approval route working with its query code", async () => {
    const calls = installFetch({
      [meUrl]: { json: freshMe },
      [claimUrl]: { json: claim },
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [approveUrl]: { status: 204 },
    });

    await renderShellAt("/enroll?code=rkuc_TEST123");

    expect(await screen.findByTestId("device-hostname")).toBeTruthy();
    expect(screen.getByTestId("device-hostname").textContent).toBe("DESKTOP-ABC123");

    await userEvent.click(screen.getByRole("button", { name: "Approve device" }));

    await waitFor(() => {
      expect(screen.getByTestId("approval-status").textContent).toMatch(/approved/i);
    });
    const approve = calls.find((call) => call.path === "/api/v1/enrollments/approve");
    expect(approve?.headers["x-csrf-token"]).toBe("csrf-token-1");
  });

  it("keeps the download page and its trial notice working", async () => {
    installFetch({ [meUrl]: { json: freshMe }, [metadataUrl]: { json: metadata } });

    await renderShellAt("/download");

    expect(await screen.findByText("Download RobloxBridge")).toBeTruthy();
    const notice = await screen.findByTestId("trial-notice");
    expect(notice.textContent).toMatch(/does not start your free trial/i);
  });

  it("signs out through the logout endpoint and mints a fresh CSRF token on the next session", async () => {
    const calls = installFetch({
      [meUrl]: [{ json: freshMe }, { status: 401 }, { json: freshMe }],
      [csrfUrl]: [
        { json: { csrf_token: "csrf-first" } },
        { json: { csrf_token: "csrf-second" } },
      ],
      [logoutUrl]: { status: 204 },
      [claimUrl]: { json: claim },
      [approveUrl]: { status: 204 },
    });

    const router = await renderShellAt("/devices");
    await screen.findByText("Signed in as Builder 1516563360");

    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));

    await waitFor(() => {
      const call = calls.find((entry) => entry.path === "/api/v1/auth/logout");
      expect(call?.headers["x-csrf-token"]).toBe("csrf-first");
    });
    expect(await screen.findByText("Continue with Roblox")).toBeTruthy();

    // Signing back in must not reuse the CSRF token from the dead session.
    await router.navigate("/enroll?code=rkuc_TEST123");
    await screen.findByTestId("device-hostname");

    await userEvent.click(screen.getByRole("button", { name: "Approve device" }));

    await waitFor(() => {
      const approve = calls.find((call) => call.path === "/api/v1/enrollments/approve");
      expect(approve?.headers["x-csrf-token"]).toBe("csrf-second");
    });
    expect(calls.filter((call) => call.path === "/api/v1/csrf")).toHaveLength(2);
  });

  it("sends cookies on every API request", async () => {
    const calls = installFetch({
      [meUrl]: [{ json: freshMe }, { status: 401 }],
      [csrfUrl]: { json: { csrf_token: "csrf-token-1" } },
      [logoutUrl]: { status: 204 },
    });

    await renderShellAt("/devices");
    await screen.findByText("Signed in as Builder 1516563360");
    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await screen.findByText("Continue with Roblox");

    expect(calls.length).toBeGreaterThan(2);
    for (const call of calls) {
      expect(call.credentials).toBe("include");
    }
  });

  it("shows the public home page with a sign-in CTA for unauthenticated visitors", async () => {
    installFetch({ [meUrl]: { status: 401 } });

    await renderShellAt("/");

    expect(
      await screen.findByRole("heading", { name: /control roblox studio/i, level: 1 }),
    ).toBeTruthy();
    const signIn = screen.getAllByRole("link", { name: "Sign in" });
    expect(signIn.length).toBeGreaterThan(0);
    expect(signIn[0].getAttribute("href")).toBe("/login");
  });

  it("offers authenticated visitors a direct dashboard CTA from the home page", async () => {
    installFetch({ [meUrl]: { json: freshMe } });

    await renderShellAt("/");

    const cta = await screen.findAllByRole("link", { name: "Open dashboard" });
    expect(cta.length).toBeGreaterThan(0);
    expect(cta[0].getAttribute("href")).toBe("/devices");
  });

  it("keeps the privacy policy public and links to the terms", async () => {
    const calls = installFetch({});

    await renderShellAt("/privacy");

    expect(await screen.findByRole("heading", { name: "Privacy Policy", level: 1 })).toBeTruthy();
    expect(screen.getByRole("link", { name: "support@rbxskuy.web.id" }).getAttribute("href")).toBe(
      "mailto:support@rbxskuy.web.id",
    );
    expect(screen.getByRole("link", { name: "Terms of Service" }).getAttribute("href")).toBe(
      "/terms",
    );
    expect(calls).toHaveLength(0);
  });

  it("keeps the terms public and links to the privacy policy", async () => {
    const calls = installFetch({});

    await renderShellAt("/terms");

    expect(await screen.findByRole("heading", { name: "Terms of Service", level: 1 })).toBeTruthy();
    expect(screen.getByRole("link", { name: "support@rbxskuy.web.id" }).getAttribute("href")).toBe(
      "mailto:support@rbxskuy.web.id",
    );
    expect(screen.getByRole("link", { name: "Privacy Policy" }).getAttribute("href")).toBe(
      "/privacy",
    );
    expect(calls).toHaveLength(0);
  });
});
