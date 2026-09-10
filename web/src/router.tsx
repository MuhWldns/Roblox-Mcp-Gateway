import { Navigate, createBrowserRouter, redirect, type RouteObject, type LoaderFunctionArgs } from "react-router";
import { UnauthorizedError, type MeResponse, getMe } from "./api/client";
import AppShell from "./layout/AppShell";
import AccountRecovery from "./routes/AccountRecovery";
import Admin from "./routes/Admin";
import AdminMetrics from "./routes/AdminMetrics";
import Connectors from "./routes/Connectors";
import Devices from "./routes/Devices";
import Diagnostics from "./routes/Diagnostics";
import Download from "./routes/Download";
import Enroll from "./routes/Enroll";
import ErrorPage from "./routes/ErrorPage";
import Home from "./routes/Home";
import License from "./routes/License";
import Login from "./routes/Login";
import Privacy from "./routes/Privacy";
import Terms from "./routes/Terms";
import Studios from "./routes/Studios";
import TrialExtension from "./routes/TrialExtension";
import DeviceTransfer from "./routes/DeviceTransfer";
import Setup from "./routes/Setup";
import Dashboard from "./routes/Dashboard";

// The session loader guards every dashboard section: an expired or missing
// browser session sends the visitor to sign in, while any other API failure
// surfaces through the shell's error boundary instead of a blank page.
export async function sessionLoader({ request }: LoaderFunctionArgs): Promise<MeResponse> {
  try {
    return await getMe();
  } catch (error) {
    if (error instanceof UnauthorizedError) {
      const url = new URL(request.url);
      throw redirect(`/login?${new URLSearchParams({ next: url.pathname + url.search })}`);
    }
    throw error;
  }
}

const dashboardSections: RouteObject[] = [
  { path: "setup", element: <Setup /> },
  { path: "dashboard", element: <Dashboard /> },
  { path: "devices", element: <Devices /> },
  { path: "studios", element: <Studios /> },
  { path: "connectors", element: <Connectors /> },
  { path: "license", element: <License /> },
  { path: "diagnostics", element: <Diagnostics /> },
  {
    path: "admin",
    children: [
      { index: true, element: <Admin /> },
		{ path: "metrics", element: <AdminMetrics /> },
      { path: "transfer", element: <DeviceTransfer /> },
      { path: "recovery", element: <AccountRecovery /> },
      { path: "extension", element: <TrialExtension /> },
    ],
  },
];

export function appRoutes(): RouteObject[] {
  return [
    {
      loader: sessionLoader,
      element: <AppShell />,
      errorElement: <ErrorPage />,
      children: [...dashboardSections],
    },
    { path: "/", element: <Home /> },
    { path: "/privacy", element: <Privacy /> },
    { path: "/terms", element: <Terms /> },
    { path: "/login", element: <Login /> },
    { path: "/download", element: <Download /> },
    { path: "/enroll", element: <Enroll /> },
    { path: "*", element: <Navigate to="/dashboard" replace /> },
  ];
}

export function createAppRouter() {
  return createBrowserRouter(appRoutes());
}
