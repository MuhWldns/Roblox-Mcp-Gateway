import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";

import Admin from "./Admin";
import AccountRecovery from "./AccountRecovery";
import DeviceTransfer from "./DeviceTransfer";
import TrialExtension from "./TrialExtension";

type MockRoute = { status?: number; json?: unknown };

type RecordedCall = {
  method: string;
  path: string;
  headers: Record<string, string>;
  body?: string;
};

// installFetch stubs window.fetch with per-route responses keyed by
// "METHOD /path" (query strings ignored) and records every call, including
// headers — the CSRF double-submit pair is asserted through them.
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
        <Route path="/admin" element={element} />
        <Route path="*" element={element} />
      </Routes>
    </MemoryRouter>,
  );
}

const csrfUrl = "GET /api/v1/csrf";
const transferPreviewUrl = "GET /api/v1/admin/users/user-1/transfer-preview";
const recoveryPreviewUrl = "GET /api/v1/admin/users/user-1/recovery-preview";
const trialPreviewUrl = "GET /api/v1/admin/users/user-1/trial-preview";
const transferUrl = "POST /api/v1/admin/transfers";
const recoveryUrl = "POST /api/v1/admin/recoveries";
const extensionUrl = "POST /api/v1/admin/trial-extensions";

const identity = { subject: "roblox-subject-1", display_name: "Builder One" };

const deviceOnline = {
  id: "device-old",
  name: "Old Laptop",
  status: "active",
  online: true,
  created_at: "2026-09-01T10:00:00Z",
  updated_at: "2026-09-04T10:00:00Z",
};
const deviceOffline = {
  id: "device-new",
  name: "New Laptop",
  status: "active",
  online: false,
  created_at: "2026-09-02T10:00:00Z",
  updated_at: "2026-09-03T10:00:00Z",
};
const license = { status: "active", device_slots: 2, active_bindings: 1 };
const connector = {
  id: "grant-1",
  client_id: "https://chatgpt.com/aip/mcp",
  client_name: "ChatGPT",
  device_id: "device-old",
  scopes: ["mcp:connect"],
  created_at: "2026-09-02T09:00:00Z",
};
const trial = {
  id: "trial-1",
  started_at: "2026-09-01T00:00:00Z",
  ends_at: "2026-09-15T00:00:00Z",
  active: true,
};

const transferPreview = {
  user_id: "user-1",
  identity,
  devices: [deviceOnline, deviceOffline],
  license,
  version: "a1b2c3d4e5f60718",
};
const recoveryPreview = {
  user_id: "user-1",
  identity,
  devices: [deviceOnline, deviceOffline],
  connectors: [connector],
  license,
  version: "b2c3d4e5f6071809",
};
const trialPreview = {
  user_id: "user-1",
  identity,
  trial,
  version: "2026-09-15T00:00:00Z",
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("admin index", () => {
  it("links the three privileged tools", () => {
    renderAt("/admin", <Admin />);

    expect(screen.getByTestId("page-admin")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Transfer a license slot" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Run an identity recovery" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Extend a trial" })).toBeTruthy();
  });
});

describe("device transfer screen", () => {
  it("previews the account state with explicit non-color status", async () => {
    installFetch({ [transferPreviewUrl]: { json: transferPreview } });

    renderAt("/admin/transfer", <DeviceTransfer />);
    await userEvent.type(screen.getByTestId("transfer-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("transfer-load"));

    expect(await screen.findByTestId("transfer-preview")).toBeTruthy();
    expect(screen.getByText(/Builder One/)).toBeTruthy();
    expect(screen.getByText("Online")).toBeTruthy();
    expect(screen.getByText("Offline")).toBeTruthy();
    expect(screen.getByText(/device slots: 2/)).toBeTruthy();
    expect(screen.getByTestId("transfer-version").textContent).toBe("a1b2c3d4e5f60718");
    expect(screen.getByLabelText("Case id")).toBeTruthy();
    expect(screen.getByLabelText("Reason")).toBeTruthy();
    expect(screen.getByLabelText("Evidence reference")).toBeTruthy();
    expect(screen.getByLabelText(/Expected version/)).toBeTruthy();
  });

  it("describes what will happen from the chosen devices", async () => {
    installFetch({ [transferPreviewUrl]: { json: transferPreview } });

    renderAt("/admin/transfer", <DeviceTransfer />);
    await userEvent.type(screen.getByTestId("transfer-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("transfer-load"));
    await screen.findByTestId("transfer-preview");

    await userEvent.selectOptions(screen.getByTestId("transfer-old-device"), "device-old");
    await userEvent.selectOptions(screen.getByTestId("transfer-new-device"), "device-new");

    const plan = screen.getByTestId("transfer-plan").textContent ?? "";
    expect(plan).toContain("device-old");
    expect(plan).toContain("device-new");
    expect(plan.toLowerCase()).toContain("closes");
  });

  it("keeps submit disabled until the typed version matches the preview", async () => {
    installFetch({ [transferPreviewUrl]: { json: transferPreview } });

    renderAt("/admin/transfer", <DeviceTransfer />);
    await userEvent.type(screen.getByTestId("transfer-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("transfer-load"));
    await screen.findByTestId("transfer-preview");

    const submit = screen.getByTestId("transfer-submit") as HTMLButtonElement;
    await userEvent.type(screen.getByTestId("transfer-case-id"), "case-1");
    await userEvent.type(screen.getByTestId("transfer-reason"), "hardware swap");
    await userEvent.type(screen.getByTestId("transfer-evidence"), "ticket-77");
    await userEvent.type(screen.getByTestId("transfer-expected-version"), "deadbeef00000000");
    expect(submit.disabled).toBe(true);

    await userEvent.clear(screen.getByTestId("transfer-expected-version"));
    await userEvent.type(screen.getByTestId("transfer-expected-version"), "a1b2c3d4e5f60718");
    await userEvent.selectOptions(screen.getByTestId("transfer-old-device"), "device-old");
    await userEvent.selectOptions(screen.getByTestId("transfer-new-device"), "device-new");
    await userEvent.type(screen.getByTestId("transfer-license-id"), "license-1");
    expect(submit.disabled).toBe(false);
  });

  it("submits the transfer with the CSRF pair and the full payload", async () => {
    const calls = installFetch({
      [transferPreviewUrl]: { json: transferPreview },
      [csrfUrl]: { json: { csrf_token: "admin-csrf-token" } },
      [transferUrl]: { status: 204 },
    });

    renderAt("/admin/transfer", <DeviceTransfer />);
    await userEvent.type(screen.getByTestId("transfer-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("transfer-load"));
    await screen.findByTestId("transfer-preview");

    await userEvent.selectOptions(screen.getByTestId("transfer-old-device"), "device-old");
    await userEvent.selectOptions(screen.getByTestId("transfer-new-device"), "device-new");
    await userEvent.type(screen.getByTestId("transfer-license-id"), "license-1");
    await userEvent.type(screen.getByTestId("transfer-case-id"), "case-200");
    await userEvent.type(screen.getByTestId("transfer-reason"), "hardware swap");
    await userEvent.type(screen.getByTestId("transfer-evidence"), "ticket-200");
    await userEvent.type(screen.getByTestId("transfer-expected-version"), "a1b2c3d4e5f60718");
    await userEvent.click(screen.getByTestId("transfer-submit"));

    await waitFor(() => expect(screen.getByTestId("transfer-success")).toBeTruthy());
    const post = calls.find((call) => call.path === "/api/v1/admin/transfers");
    expect(post).toBeTruthy();
    expect(post?.headers["x-csrf-token"]).toBe("admin-csrf-token");
    const payload = JSON.parse(post?.body ?? "{}");
    expect(payload).toEqual({
      user_id: "user-1",
      license_id: "license-1",
      old_device_id: "device-old",
      new_device_id: "device-new",
      expected_version: "a1b2c3d4e5f60718",
      case_id: "case-200",
      reason: "hardware swap",
      evidence_ref: "ticket-200",
    });
  });

  it("surfaces the stale-version conflict as an alert", async () => {
    installFetch({
      [transferPreviewUrl]: { json: transferPreview },
      [csrfUrl]: { json: { csrf_token: "admin-csrf-token" } },
      [transferUrl]: { status: 409 },
    });

    renderAt("/admin/transfer", <DeviceTransfer />);
    await userEvent.type(screen.getByTestId("transfer-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("transfer-load"));
    await screen.findByTestId("transfer-preview");
    await userEvent.selectOptions(screen.getByTestId("transfer-old-device"), "device-old");
    await userEvent.selectOptions(screen.getByTestId("transfer-new-device"), "device-new");
    await userEvent.type(screen.getByTestId("transfer-license-id"), "license-1");
    await userEvent.type(screen.getByTestId("transfer-case-id"), "case-300");
    await userEvent.type(screen.getByTestId("transfer-reason"), "hardware swap");
    await userEvent.type(screen.getByTestId("transfer-evidence"), "ticket-300");
    await userEvent.type(screen.getByTestId("transfer-expected-version"), "a1b2c3d4e5f60718");
    await userEvent.click(screen.getByTestId("transfer-submit"));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("changed since the preview");
  });

  it("renders an explicit no-access state for non-admins", async () => {
    installFetch({ [transferPreviewUrl]: { status: 403 } });

    renderAt("/admin/transfer", <DeviceTransfer />);
    await userEvent.type(screen.getByTestId("transfer-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("transfer-load"));

    expect(await screen.findByTestId("transfer-forbidden")).toBeTruthy();
  });
});

describe("account recovery screen", () => {
  it("previews the revocable surface and states the trial stays untouched", async () => {
    installFetch({ [recoveryPreviewUrl]: { json: recoveryPreview } });

    renderAt("/admin/recovery", <AccountRecovery />);
    await userEvent.type(screen.getByTestId("recovery-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("recovery-load"));

    expect(await screen.findByTestId("recovery-preview")).toBeTruthy();
    expect(screen.getByText(/ChatGPT/)).toBeTruthy();
    const plan = screen.getByTestId("recovery-plan").textContent ?? "";
    expect(plan.toLowerCase()).toContain("session");
    expect(plan.toLowerCase()).toContain("connector");
    expect(plan.toLowerCase()).toContain("credential");
    expect(plan.toLowerCase()).toContain("trial");
    expect(plan.toLowerCase()).toContain("not changed");
    expect(screen.getByLabelText("Case id")).toBeTruthy();
    expect(screen.getByLabelText("Reason")).toBeTruthy();
    expect(screen.getByLabelText("Evidence reference")).toBeTruthy();
    expect(screen.getByLabelText(/Expected version/)).toBeTruthy();
  });

  it("submits the recovery with the CSRF pair and the full payload", async () => {
    const calls = installFetch({
      [recoveryPreviewUrl]: { json: recoveryPreview },
      [csrfUrl]: { json: { csrf_token: "admin-csrf-token" } },
      [recoveryUrl]: { status: 204 },
    });

    renderAt("/admin/recovery", <AccountRecovery />);
    await userEvent.type(screen.getByTestId("recovery-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("recovery-load"));
    await screen.findByTestId("recovery-preview");

    await userEvent.type(screen.getByTestId("recovery-new-identity"), "identity-new-1");
    await userEvent.type(screen.getByTestId("recovery-case-id"), "case-500");
    await userEvent.type(screen.getByTestId("recovery-reason"), "stolen account");
    await userEvent.type(screen.getByTestId("recovery-evidence"), "evidence-9");
    await userEvent.type(screen.getByTestId("recovery-expected-version"), "b2c3d4e5f6071809");
    await userEvent.click(screen.getByTestId("recovery-submit"));

    await waitFor(() => expect(screen.getByTestId("recovery-success")).toBeTruthy());
    const post = calls.find((call) => call.path === "/api/v1/admin/recoveries");
    expect(post?.headers["x-csrf-token"]).toBe("admin-csrf-token");
    const payload = JSON.parse(post?.body ?? "{}");
    expect(payload).toEqual({
      user_id: "user-1",
      expected_version: "b2c3d4e5f6071809",
      case_id: "case-500",
      reason: "stolen account",
      evidence_ref: "evidence-9",
      new_identity_id: "identity-new-1",
    });
  });

  it("surfaces a server failure as an alert", async () => {
    installFetch({
      [recoveryPreviewUrl]: { json: recoveryPreview },
      [csrfUrl]: { json: { csrf_token: "admin-csrf-token" } },
      [recoveryUrl]: { status: 500 },
    });

    renderAt("/admin/recovery", <AccountRecovery />);
    await userEvent.type(screen.getByTestId("recovery-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("recovery-load"));
    await screen.findByTestId("recovery-preview");
    await userEvent.type(screen.getByTestId("recovery-case-id"), "case-501");
    await userEvent.type(screen.getByTestId("recovery-reason"), "stolen account");
    await userEvent.type(screen.getByTestId("recovery-evidence"), "evidence-9");
    await userEvent.type(screen.getByTestId("recovery-expected-version"), "b2c3d4e5f6071809");
    await userEvent.click(screen.getByTestId("recovery-submit"));

    expect(await screen.findByRole("alert")).toBeTruthy();
  });
});

describe("trial extension screen", () => {
  it("previews the trial and its current expiry", async () => {
    installFetch({ [trialPreviewUrl]: { json: trialPreview } });

    renderAt("/admin/extension", <TrialExtension />);
    await userEvent.type(screen.getByTestId("extension-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("extension-load"));

    expect(await screen.findByTestId("extension-preview")).toBeTruthy();
    expect(screen.getByText("trial-1")).toBeTruthy();
    expect(screen.getAllByText(/2026-09-15T00:00:00Z/).length).toBeGreaterThan(0);
    const plan = screen.getByTestId("extension-plan").textContent ?? "";
    expect(plan.toLowerCase()).toContain("same entitlement");
    expect(screen.getByLabelText("Case id")).toBeTruthy();
    expect(screen.getByLabelText("Reason")).toBeTruthy();
    expect(screen.getByLabelText("Evidence reference")).toBeTruthy();
    expect(screen.getByLabelText(/Expected version/)).toBeTruthy();
  });

  it("submits the extension with the CSRF pair and the full payload", async () => {
    const calls = installFetch({
      [trialPreviewUrl]: { json: trialPreview },
      [csrfUrl]: { json: { csrf_token: "admin-csrf-token" } },
      [extensionUrl]: { status: 204 },
    });

    renderAt("/admin/extension", <TrialExtension />);
    await userEvent.type(screen.getByTestId("extension-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("extension-load"));
    await screen.findByTestId("extension-preview");

    await userEvent.type(screen.getByTestId("extension-entitlement-id"), "trial-1");
    await userEvent.type(screen.getByTestId("extension-new-ends-at"), "2026-09-25T00:00:00Z");
    await userEvent.type(screen.getByTestId("extension-case-id"), "case-700");
    await userEvent.type(screen.getByTestId("extension-reason"), "goodwill extension");
    await userEvent.type(screen.getByTestId("extension-evidence"), "ticket-700");
    await userEvent.type(screen.getByTestId("extension-expected-version"), "2026-09-15T00:00:00Z");
    await userEvent.click(screen.getByTestId("extension-submit"));

    await waitFor(() => expect(screen.getByTestId("extension-success")).toBeTruthy());
    const post = calls.find((call) => call.path === "/api/v1/admin/trial-extensions");
    expect(post?.headers["x-csrf-token"]).toBe("admin-csrf-token");
    const payload = JSON.parse(post?.body ?? "{}");
    expect(payload).toEqual({
      user_id: "user-1",
      entitlement_id: "trial-1",
      new_ends_at: "2026-09-25T00:00:00Z",
      expected_version: "2026-09-15T00:00:00Z",
      case_id: "case-700",
      reason: "goodwill extension",
      evidence_ref: "ticket-700",
    });
  });

  it("keeps submit disabled when the typed expiry is not the current one", async () => {
    installFetch({ [trialPreviewUrl]: { json: trialPreview } });

    renderAt("/admin/extension", <TrialExtension />);
    await userEvent.type(screen.getByTestId("extension-user-id"), "user-1");
    await userEvent.click(screen.getByTestId("extension-load"));
    await screen.findByTestId("extension-preview");

    await userEvent.type(screen.getByTestId("extension-entitlement-id"), "trial-1");
    await userEvent.type(screen.getByTestId("extension-new-ends-at"), "2026-09-25T00:00:00Z");
    await userEvent.type(screen.getByTestId("extension-case-id"), "case-800");
    await userEvent.type(screen.getByTestId("extension-reason"), "goodwill extension");
    await userEvent.type(screen.getByTestId("extension-evidence"), "ticket-800");
    await userEvent.type(screen.getByTestId("extension-expected-version"), "2026-09-14T00:00:00Z");
    expect((screen.getByTestId("extension-submit") as HTMLButtonElement).disabled).toBe(true);
  });
});
