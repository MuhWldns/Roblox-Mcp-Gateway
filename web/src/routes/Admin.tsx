import { Link } from "react-router";

// Admin is the privileged-tools index. The tools themselves gate on the
// server: every preview and mutation answers 403 for non-administrators, so
// the links stay visible and each screen renders an explicit no-access state.
export default function Admin() {
  return (
    <section data-testid="page-admin" aria-labelledby="admin-title" className="animate-[pageEnter_200ms_ease]">
      <h2 id="admin-title" className="text-xl font-semibold text-navy mb-1">Admin</h2>
      <p className="text-text-secondary mb-6 max-w-[70ch]">
        Privileged support actions. Every action needs a case id, a reason, an
        evidence reference, and the version of the state you previewed.
      </p>
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
