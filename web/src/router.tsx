import { Navigate, createBrowserRouter, redirect, type RouteObject } from "react-router";
import { UnauthorizedError, type MeResponse, getMe } from "./api/client";
import AppShell from "./layout/AppShell";
import AccountRecovery from "./routes/AccountRecovery";
import Admin from "./routes/Admin";
import Connectors from "./routes/Connectors";
import Devices from "./routes/Devices";
import Diagnostics from "./routes/Diagnostics";
import Download from "./routes/Download";
import Enroll from "./routes/Enroll";
import ErrorPage from "./routes/ErrorPage";
import License from "./routes/License";
import Login from "./routes/Login";
import Studios from "./routes/Studios";
import TrialExtension from "./routes/TrialExtension";
import DeviceTransfer from "./routes/DeviceTransfer";

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

const dashboardSections: RouteObject[] = [
  { path: "devices", element: <Devices /> },
  { path: "studios", element: <Studios /> },
  { path: "connectors", element: <Connectors /> },
  { path: "license", element: <License /> },
  { path: "diagnostics", element: <Diagnostics /> },
  {
    path: "admin",
    children: [
      { index: true, element: <Admin /> },
      { path: "transfer", element: <DeviceTransfer /> },
      { path: "recovery", element: <AccountRecovery /> },
      { path: "extension", element: <TrialExtension /> },
    ],
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
