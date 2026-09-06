import type { ReactNode } from "react";
import { Link } from "react-router";

interface LegalLayoutProps {
  title: string;
  description: string;
  children: ReactNode;
}

export const legalEffectiveDate = "September 5, 2026";
export const legalContactEmail = "support@rbxskuy.web.id";

export default function LegalLayout({ title, description, children }: LegalLayoutProps) {
  return (
    <div className="min-h-screen bg-surface text-navy">
      <header className="bg-navy text-white border-b border-navy-light">
        <div className="max-w-[1120px] mx-auto px-6 py-5 flex items-center justify-between gap-6 max-sm:px-4">
          <Link to="/login" className="font-bold tracking-tight text-lg text-white no-underline">
            RobloxKit
          </Link>
          <nav aria-label="Legal documents" className="flex items-center gap-5 text-sm">
            <Link to="/privacy" className="inline-flex min-h-11 items-center px-2 -mx-2 text-white/70 hover:text-white underline-offset-4">
              Privacy
            </Link>
            <Link to="/terms" className="inline-flex min-h-11 items-center px-2 -mx-2 text-white/70 hover:text-white underline-offset-4">
              Terms
            </Link>
          </nav>
        </div>
      </header>

      <main className="max-w-[1120px] mx-auto px-6 py-14 grid grid-cols-[minmax(0,1fr)_220px] gap-16 items-start max-lg:grid-cols-1 max-sm:px-4 max-sm:py-9">
        <article className="max-w-[74ch] min-w-0">
          <header className="pb-8 mb-10 border-b border-border">
            <h1 className="text-[clamp(2rem,5vw,3.25rem)] leading-[1.05] tracking-[-0.035em] font-bold text-navy mb-4">
              {title}
            </h1>
            <p className="text-lg leading-relaxed text-text-secondary mb-5">{description}</p>
            <p className="text-sm text-text-secondary m-0">
              Effective date: <time dateTime="2026-09-05">{legalEffectiveDate}</time>
            </p>
          </header>
          <div className="legal-copy">{children}</div>
        </article>

        <aside className="sticky top-8 border-t-2 border-red pt-4 max-lg:static max-lg:max-w-[74ch]" aria-label="Document information">
          <p className="text-xs font-semibold uppercase tracking-[0.08em] text-text-secondary mb-2">Contact</p>
          <p className="text-sm font-medium text-navy break-all m-0">
            {legalContactEmail}
          </p>
          <p className="text-xs leading-relaxed text-text-secondary mt-4 mb-0">
            Questions about these terms or your data are handled by RobloxKit at this address.
          </p>
        </aside>
      </main>

      <footer className="border-t border-border bg-white">
        <div className="max-w-[1120px] mx-auto px-6 py-6 flex items-center justify-between gap-4 text-sm text-text-secondary max-sm:px-4 max-sm:flex-col max-sm:items-start">
          <span>© 2026 RobloxKit</span>
          <div className="flex gap-5">
            <Link to="/privacy" className="inline-flex min-h-11 items-center hover:text-red underline-offset-4">Privacy</Link>
            <Link to="/terms" className="inline-flex min-h-11 items-center hover:text-red underline-offset-4">Terms</Link>
          </div>
        </div>
      </footer>
    </div>
  );
}
