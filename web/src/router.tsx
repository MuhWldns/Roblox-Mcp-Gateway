import { Navigate, createBrowserRouter, redirect, type RouteObject } from "react-router";
import { UnauthorizedError, type MeResponse, getMe } from "./api/client";
import AppShell from "./layout/AppShell";
import Download from "./routes/Download";
import Enroll from "./routes/Enroll";
import ErrorPage from "./routes/ErrorPage";
import Login from "./routes/Login";

// The session loader guards every dashboard section: an expired or missing
// browser session sends the visitor to sign in, while any other API failure
// surfaces through the shell's error boundary instead of a blank page.
export async function sessionLoader(): Promise<MeResponse> {
  try {
    return await getMe();
  } catch (error) {
    if (error instanceof UnauthorizedError) {
      throw redirect("/login");
    }
    throw error;
  }
}

type ShellSectionProps = {
  slug: string;
  title: string;
  description: string;
};

// Placeholder sections render a meaningful empty state until their full
// screens land; each keeps a stable test id for navigation assertions.
function ShellSection({ slug, title, description }: ShellSectionProps) {
  return (
    <section data-testid={`page-${slug}`}>
      <h2>{title}</h2>
      <p>{description}</p>
    </section>
  );
}

const dashboardSections: RouteObject[] = [
  {
    path: "devices",
    element: (
      <ShellSection
        slug="devices"
        title="Devices"
        description="Bridges that finished enrollment will appear here."
      />
    ),
  },
  {
    path: "studios",
    element: (
      <ShellSection
        slug="studios"
        title="Studios"
        description="Roblox Studio clients connected through your Bridge will appear here."
      />
    ),
  },
  {
    path: "connectors",
    element: (
      <ShellSection
        slug="connectors"
        title="Connectors"
        description="AI connectors such as ChatGPT and Claude will appear here."
      />
    ),
  },
  {
    path: "license",
    element: (
      <ShellSection
        slug="license"
        title="License"
        description="Your license and free-trial details will appear here."
      />
    ),
  },
  {
    path: "diagnostics",
    element: (
      <ShellSection
        slug="diagnostics"
        title="Diagnostics"
        description="Bridge connection health and diagnostics will appear here."
      />
    ),
  },
  {
    path: "admin",
    element: (
      <ShellSection
        slug="admin"
        title="Admin"
        description="Administration tools will appear here."
      />
    ),
  },
];

export function appRoutes(): RouteObject[] {
  return [
    {
      path: "/",
      loader: sessionLoader,
      element: <AppShell />,
      errorElement: <ErrorPage />,
      children: [{ index: true, element: <Navigate to="/devices" replace /> }, ...dashboardSections],
    },
    { path: "/login", element: <Login /> },
    { path: "/download", element: <Download /> },
    { path: "/enroll", element: <Enroll /> },
    { path: "*", element: <Navigate to="/devices" replace /> },
  ];
}

export function createAppRouter() {
  return createBrowserRouter(appRoutes());
}
