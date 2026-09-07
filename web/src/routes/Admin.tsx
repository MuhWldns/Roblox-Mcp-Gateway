import { useEffect, useState } from "react";
import { Link, Navigate } from "react-router";
import { getAdminUsers, ApiError, UnauthorizedError, type AdminUser } from "../api/client";

// Admin is the privileged-tools index. The tools themselves gate on the
// server: every preview and mutation answers 403 for non-administrators, so
// the links stay visible and each screen renders an explicit no-access state.
export default function Admin() {
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [next, setNext] = useState("");
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState("");
  const [denied, setDenied] = useState(false);
  async function load(after = "") {
    setBusy(true); setError("");
    try {
      const page = await getAdminUsers(after);
      setUsers(previous => after ? [...previous, ...page.users] : page.users);
      setNext(page.next);
    } catch (err) {
      if (err instanceof UnauthorizedError) setDenied(true);
      else setError(err instanceof ApiError && err.status === 403 ? "Administrator access required." : "Could not load users. Try again.");
    } finally { setBusy(false); }
  }
  useEffect(() => { void load(); }, []);
  if (denied) return <Navigate to="/login" replace />;
  return (
    <section data-testid="page-admin" aria-labelledby="admin-title" className="animate-[pageEnter_200ms_ease]">
      <h2 id="admin-title" className="text-xl font-semibold text-navy mb-1">Admin</h2>
      <p className="text-text-secondary mb-6 max-w-[70ch]">
        Privileged support actions. Every action needs a case id, a reason, an
        evidence reference, and the version of the state you previewed.
      </p>
      <h3 className="text-lg font-semibold text-navy mb-3">Users</h3>
      {error && <p role="alert">{error}</p>}
      {!busy && !error && users.length === 0 && <p>No registered users yet.</p>}
      <ul className="list-none p-0 mb-6 divide-y divide-border border border-border rounded-lg bg-white">
        {users.map(user => <li key={user.id} className="p-4 flex flex-wrap items-center justify-between gap-4">
          <div className="min-w-0"><strong>{user.display_name || "Unnamed account"}</strong>
            <p className="text-sm text-text-secondary break-all m-0">Roblox ID: {user.subject || "Unavailable"} · {user.id}</p></div>
          <div className="flex flex-wrap gap-4 text-sm font-semibold">
            <Link to={`/admin/extension?user_id=${encodeURIComponent(user.id)}`}>Extend trial</Link>
            <Link className="text-red" to={`/admin/recovery?user_id=${encodeURIComponent(user.id)}`}>Revoke access</Link>
          </div>
        </li>)}
      </ul>
      {busy && <p role="status">Loading users…</p>}
      {!busy && (next || error) && <button type="button" onClick={() => void load(next)}>{error ? "Retry" : "Load more users"}</button>}
      <ul className="list-none p-0 m-0 grid gap-4">
        <li className="bg-white border border-border rounded-lg p-5">
          <Link className="text-base font-semibold text-navy hover:text-red transition-colors" to="/admin/transfer">Transfer a license slot</Link>
          <p className="text-sm text-text-secondary mt-1 mb-0">Move an active paid-license slot from one device to another.</p>
        </li>
        <li className="bg-white border border-border rounded-lg p-5">
          <Link className="text-base font-semibold text-navy hover:text-red transition-colors" to="/admin/recovery">Run an identity recovery</Link>
          <p className="text-sm text-text-secondary mt-1 mb-0">Revoke every session, connector grant and token, and device credential of an account.</p>
        </li>
        <li className="bg-white border border-border rounded-lg p-5">
          <Link className="text-base font-semibold text-navy hover:text-red transition-colors" to="/admin/extension">Extend a trial</Link>
          <p className="text-sm text-text-secondary mt-1 mb-0">Move the expiry of an existing trial entitlement later without creating a second trial.</p>
        </li>
      </ul>
    </section>
  );
}
