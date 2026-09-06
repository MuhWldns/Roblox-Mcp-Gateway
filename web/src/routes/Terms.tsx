import { Link } from "react-router";
import LegalLayout, { legalContactEmail } from "../components/LegalLayout";

export default function Terms() {
  return (
    <LegalLayout
      title="Terms of Service"
      description="The rules that apply when you use the RobloxKit dashboard, gateway, and Windows Bridge."
    >
      <section>
        <h2>1. Acceptance</h2>
        <p>
          These Terms of Service form an agreement between you and RobloxKit. By accessing or using RobloxKit, you agree to these Terms and the <Link to="/privacy">Privacy Policy</Link>. If you do not agree, do not use the service.
        </p>
      </section>

      <section>
        <h2>2. Eligibility and authority</h2>
        <p>
          You must meet the minimum age required to consent to online services in your jurisdiction. If you use RobloxKit for an organization or another person, you confirm that you have authority to accept these Terms and operate the connected Roblox account, device, and Studio sessions.
        </p>
      </section>

      <section>
        <h2>3. The service</h2>
        <p>
          RobloxKit provides a dashboard, cloud gateway, and Windows Bridge that route authorized Model Context Protocol requests from a supported AI client to the official Roblox Studio MCP running on your selected device. Your computer opens an outbound secure WebSocket connection; RobloxKit does not provide general-purpose remote shell access.
        </p>
        <p>
          RobloxKit is independent and is not affiliated with, endorsed by, or sponsored by Roblox Corporation, OpenAI, or Anthropic. Those services and the official Roblox Studio MCP remain third-party products governed by their own terms and policies.
        </p>
      </section>

      <section>
        <h2>4. Accounts, devices, and credentials</h2>
        <p>
          You are responsible for access to your Roblox account, signed-in browsers, enrolled devices, AI client accounts, and local computer. Keep credentials confidential, use devices you control, and promptly revoke devices or connectors you no longer trust. You may not transfer, expose, share, extract, or circumvent service credentials or access controls.
        </p>
      </section>

      <section>
        <h2>5. Acceptable use</h2>
        <p>You must not use RobloxKit to:</p>
        <ul>
          <li>violate law, Roblox rules, third-party rights, or applicable platform policies;</li>
          <li>access an account, device, Studio session, experience, or data without authorization;</li>
          <li>bypass licenses, trials, device limits, scopes, rate limits, or security controls;</li>
          <li>probe, disrupt, overload, reverse engineer, or introduce malicious code into the service;</li>
          <li>misrepresent automated actions as approved by another person; or</li>
          <li>use generated or relayed actions without reviewing their effects where human review is reasonably required.</li>
        </ul>
      </section>

      <section>
        <h2>6. AI-assisted actions and unknown outcomes</h2>
        <p>
          AI-generated tool requests can be incomplete, incorrect, or harmful. You remain responsible for instructions you authorize and changes made in Roblox Studio. Network or process failures can leave the result of a tool call unknown. RobloxKit does not automatically replay a call whose side effects may already have occurred; verify project state before trying again.
        </p>
      </section>

      <section>
        <h2>7. Trials, licenses, and availability</h2>
        <p>
          Access to protected features may require an active trial or license and may be limited by account, device, connector scope, usage, or capacity. Trial eligibility and duration, license device slots, and transfer or recovery rules are enforced by the service. Reinstalling the Bridge, revoking a device, or creating another account does not reset consumed eligibility.
        </p>
        <p>
          RobloxKit may change, suspend, or discontinue features to maintain security, comply with law or third-party requirements, prevent abuse, or operate the service. We will use reasonable efforts to avoid unnecessary interruption but do not guarantee uninterrupted availability.
        </p>
      </section>

      <section>
        <h2>8. Your content and permissions</h2>
        <p>
          You retain rights in content and projects you own. You grant RobloxKit the limited permission necessary to transmit your selected requests and responses, operate the connector and Bridge, and provide security and diagnostic functions. You confirm that you have the rights needed for any content or project you process through the service.
        </p>
      </section>

      <section>
        <h2>9. Suspension and termination</h2>
        <p>
          You may stop using RobloxKit and revoke devices and connectors at any time. RobloxKit may restrict or terminate access for violations of these Terms, security threats, suspected unauthorized use, legal requirements, or material risk to the service or others. Provisions that by their nature should survive termination—including responsibility, disclaimers, liability limits, and dispute terms—remain effective.
        </p>
      </section>

      <section>
        <h2>10. Disclaimers</h2>
        <p>
          To the maximum extent permitted by law, RobloxKit is provided “as is” and “as available,” without warranties of uninterrupted operation, compatibility, accuracy, merchantability, fitness for a particular purpose, or non-infringement. Nothing in these Terms excludes warranties or rights that cannot lawfully be excluded.
        </p>
      </section>

      <section>
        <h2>11. Limitation of liability</h2>
        <p>
          To the maximum extent permitted by law, RobloxKit will not be liable for indirect, incidental, special, consequential, exemplary, or punitive damages; lost profits, data, goodwill, or opportunities; or unauthorized or unintended changes to a Roblox project arising from use of or inability to use the service. Liability that cannot legally be limited remains unaffected.
        </p>
      </section>

      <section>
        <h2>12. Governing law and disputes</h2>
        <p>
          These Terms are governed by the laws of the Republic of Indonesia, without regard to conflict-of-law principles. Before filing a formal claim, you agree to contact RobloxKit and make a good-faith effort to resolve the dispute informally, except where applicable law permits immediate relief.
        </p>
      </section>

      <section>
        <h2>13. Changes to these Terms</h2>
        <p>
          RobloxKit may update these Terms as the service or applicable requirements change. Revised Terms will be posted here with a new effective date. Continuing to use the service after revised Terms take effect constitutes acceptance where permitted by law.
        </p>
      </section>

      <section>
        <h2>14. Contact</h2>
        <p>
          Questions about these Terms may be sent to <a href={`mailto:${legalContactEmail}`}>{legalContactEmail}</a>.
        </p>
      </section>
    </LegalLayout>
  );
}
