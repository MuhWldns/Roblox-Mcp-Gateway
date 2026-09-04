import { NavLink, Outlet, useLoaderData, useNavigate } from "react-router";
import { type MeResponse, logout } from "../api/client";

const sections = [
  { to: "/devices", label: "Devices" },
  { to: "/studios", label: "Studios" },
  { to: "/connectors", label: "Connectors" },
  { to: "/license", label: "License" },
  { to: "/diagnostics", label: "Diagnostics" },
  { to: "/admin", label: "Admin" },
] as const;

export default function AppShell() {
  const me = useLoaderData() as MeResponse;
  const navigate = useNavigate();

  // Sign-out posts through the CSRF-protected logout endpoint; the visitor
  // lands on the login page either way — the session cookie is the source of
  // truth, not the navigation.
  async function signOut() {
    try {
      await logout();
    } finally {
      navigate("/login");
    }
  }

  return (
    <div>
      <header>
        <h1>RobloxKit</h1>
        <p data-testid="shell-user">Signed in as {me.display_name}</p>
        <button type="button" onClick={signOut}>
          Sign out
        </button>
      </header>
      <nav data-testid="app-nav" aria-label="Dashboard sections">
        <ul>
          {sections.map((section) => (
            <li key={section.to}>
              <NavLink to={section.to}>{section.label}</NavLink>
            </li>
          ))}
        </ul>
      </nav>
      <main>
        <Outlet />
      </main>
    </div>
  );
}
