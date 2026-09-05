import { Link, NavLink, Outlet, useLoaderData, useNavigate } from "react-router";
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

  async function signOut() {
    try {
      await logout();
    } finally {
      navigate("/login");
    }
  }

  return (
    <div className="grid grid-cols-[240px_1fr] grid-rows-[56px_1fr] min-h-screen max-md:grid-cols-[1fr] max-md:grid-rows-[auto_auto_1fr]">
      {/* Sidebar */}
      <aside className="bg-navy text-white flex flex-col py-6 fixed top-0 left-0 bottom-0 w-[240px] overflow-y-auto z-10 max-md:static max-md:w-full max-md:flex-row max-md:py-3 max-md:overflow-x-auto">
        <div className="px-5 pb-6 border-b border-navy-light mb-4 max-md:hidden">
          <h1 className="text-lg font-bold text-white tracking-tight m-0">RobloxKit</h1>
          <span className="block text-[11px] font-medium text-text-muted uppercase tracking-wider mt-0.5">BY RBX</span>
        </div>
        <nav className="flex-1 px-3 max-md:flex max-md:flex-1" data-testid="app-nav" aria-label="Dashboard sections">
          <ul className="list-none p-0 m-0 max-md:flex max-md:gap-1">
            {sections.map((section) => (
              <li key={section.to}>
                <NavLink
                  to={section.to}
                  className={({ isActive }) =>
                    `flex items-center gap-3 px-3 py-2 rounded-md text-sm font-medium transition-colors mb-0.5 max-md:px-3 max-md:py-1 max-md:text-xs max-md:whitespace-nowrap ${
                      isActive
                        ? "text-white bg-navy-light font-semibold"
                        : "text-white/70 hover:text-white hover:bg-navy-light"
                    }`
                  }
                >
                  {section.label}
                </NavLink>
              </li>
            ))}
          </ul>
        </nav>
        <nav aria-label="Legal" className="px-5 pt-4 border-t border-navy-light flex gap-4 text-[11px] shrink-0 max-md:border-t-0 max-md:border-l max-md:pt-0 max-md:items-center max-md:px-4">
          <Link to="/privacy" className="text-white/60 hover:text-white whitespace-nowrap">Privacy Policy</Link>
          <Link to="/terms" className="text-white/60 hover:text-white whitespace-nowrap">Terms of Service</Link>
        </nav>
      </aside>

      {/* Header */}
      <header className="bg-white border-b border-border flex items-center justify-end px-6 gap-4 sticky top-0 z-5">
        <span className="text-[13px] text-text-secondary" data-testid="shell-user">
          Signed in as {me.display_name}
        </span>
        <button
          type="button"
          onClick={signOut}
          className="text-[13px] px-3 py-1 min-h-[30px] border border-border rounded-md text-navy bg-transparent hover:bg-surface-alt transition-colors cursor-pointer"
        >
          Sign out
        </button>
      </header>

      {/* Content */}
      <main className="py-8 px-10 max-w-[960px] w-full max-md:px-4 max-md:py-4">
        <Outlet />
      </main>
    </div>
  );
}