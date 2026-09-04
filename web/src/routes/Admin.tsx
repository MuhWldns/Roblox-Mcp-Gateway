import { Link } from "react-router";

// Admin is the privileged-tools index. The tools themselves gate on the
// server: every preview and mutation answers 403 for non-administrators, so
// the links stay visible and each screen renders an explicit no-access state.
export default function Admin() {
  return (
    <section data-testid="page-admin" aria-labelledby="admin-title">
      <h2 id="admin-title">Admin</h2>
      <p>
        Privileged support actions. Every action needs a case id, a reason, an
        evidence reference, and the version of the state you previewed.
      </p>
      <ul>
        <li>
          <Link to="/admin/transfer">Transfer a license slot</Link> — move an
          active paid-license slot from one device to another.
        </li>
        <li>
          <Link to="/admin/recovery">Run an identity recovery</Link> — revoke
          every session, connector grant and token, and device credential of an
          account.
        </li>
        <li>
          <Link to="/admin/extension">Extend a trial</Link> — move the expiry
          of an existing trial entitlement later. No second trial is created.
        </li>
      </ul>
    </section>
  );
}
