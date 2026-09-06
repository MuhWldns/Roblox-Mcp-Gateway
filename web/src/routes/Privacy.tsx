import { Link } from "react-router";
import LegalLayout, { legalContactEmail } from "../components/LegalLayout";

export default function Privacy() {
  return (
    <LegalLayout
      title="Privacy Policy"
      description="How RobloxKit collects, uses, protects, and gives you control over information used to connect your AI client to Roblox Studio."
    >
      <section>
        <h2>1. Who we are</h2>
        <p>
          RobloxKit operates the RobloxKit gateway, dashboard, and Windows Bridge. RobloxKit is an independent service and is not affiliated with, endorsed by, or sponsored by Roblox Corporation, OpenAI, or Anthropic.
        </p>
      </section>

      <section>
        <h2>2. Information we process</h2>
        <p>We process information needed to provide and secure the service:</p>
        <ul>
          <li><strong>Account information:</strong> your Roblox account identifier and display name received during Roblox sign-in.</li>
          <li><strong>Device information:</strong> a service-generated device identifier, device name, hostname, operating system, Bridge version, connection status, and operational timestamps.</li>
          <li><strong>Studio connection information:</strong> Studio session identifiers, the device hosting each session, and session status and timestamps.</li>
          <li><strong>Connector information:</strong> the AI client name, approved scopes, selected device or Studio session, and grant and revocation timestamps.</li>
          <li><strong>Service and security records:</strong> request identifiers, audit events, rate-limit events, error categories, usage counts, and connection diagnostics.</li>
          <li><strong>Session data:</strong> secure cookies used to maintain your signed-in dashboard session and protect state-changing requests.</li>
        </ul>
        <p>
          RobloxKit stores digests of service-issued credentials and tokens rather than their plaintext values. Roblox account, device, session, and connector credentials remain separate and are not interchangeable.
        </p>
      </section>

      <section>
        <h2>3. How we use information</h2>
        <p>We use this information to:</p>
        <ul>
          <li>authenticate you and maintain your dashboard session;</li>
          <li>enroll, identify, route to, diagnose, and revoke your Bridge devices;</li>
          <li>authorize connector access to the device, Studio session, and scopes you select;</li>
          <li>enforce trial, license, security, and abuse-prevention rules;</li>
          <li>operate, troubleshoot, and protect the gateway; and</li>
          <li>respond to support, privacy, and account-security requests.</li>
        </ul>
      </section>

      <section>
        <h2>4. Cookies and local storage</h2>
        <p>
          RobloxKit uses secure, host-only cookies for authentication and cross-site request forgery protection. Authentication cookies are marked Secure and HttpOnly. The service does not place device credentials, connector tokens, or Roblox provider tokens in browser local storage.
        </p>
      </section>

      <section>
        <h2>5. When information is shared</h2>
        <p>
          Information is disclosed only as needed to operate the service, comply with law, protect users and the service, or complete an action you request. This can include Roblox for account authentication, your selected AI client for the connector flow, and infrastructure providers that host or transport RobloxKit data.
        </p>
        <p>
          Your tool requests and responses travel through RobloxKit to the Bridge on your selected device. The Bridge communicates locally with the official Roblox Studio MCP. Your use of Roblox, ChatGPT, Claude, and other third-party services is also governed by those providers' policies.
        </p>
      </section>

      <section>
        <h2>6. Data retention</h2>
        <p>
          RobloxKit retains account, device, connector, security, and audit records for as long as reasonably necessary to provide the service, protect it from abuse, meet legal obligations, resolve disputes, and enforce agreements. Retention periods can differ by record type. Revocation may preserve security and audit history rather than immediately deleting it.
        </p>
      </section>

      <section>
        <h2>7. Security</h2>
        <p>
          RobloxKit uses encrypted transport, restricted cookies, hashed or keyed credential storage, bounded access, revocation controls, rate limiting, and audit records. The Windows Bridge protects its device credential using Windows data protection. No system is completely secure, so you should promptly report suspected compromise and revoke affected devices or connectors.
        </p>
      </section>

      <section>
        <h2>8. Your choices and requests</h2>
        <p>
          The dashboard lets you review and revoke devices and connector access. To request access to, correction of, or deletion of personal information, or to ask a privacy question, email <a href={`mailto:${legalContactEmail}`}>{legalContactEmail}</a>. We may need to verify your identity before completing a request, and some records may be retained where required for security, legal, or legitimate operational purposes.
        </p>
      </section>

      <section>
        <h2>9. International processing and children</h2>
        <p>
          RobloxKit and its infrastructure providers may process information in countries other than your own. RobloxKit is not directed to children below the minimum age required to consent to online services in their jurisdiction. A parent or guardian who believes a child provided information without appropriate permission should contact us.
        </p>
      </section>

      <section>
        <h2>10. Changes to this policy</h2>
        <p>
          We may update this Privacy Policy when the service, applicable requirements, or our data practices change. We will post the revised policy here and update its effective date. Material changes may also be communicated through the service where appropriate.
        </p>
      </section>

      <section>
        <h2>11. Contact</h2>
        <p>
          Contact RobloxKit using the address shown on this page. By using RobloxKit, you also agree to the <Link to="/terms">Terms of Service</Link>.
        </p>
      </section>
    </LegalLayout>
  );
}
