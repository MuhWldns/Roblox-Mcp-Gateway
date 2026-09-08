import { Link } from "react-router";
import Metadata from "../components/Metadata";
import DownloadHeroSection from "../routes/DownloadHero";

export default function Download() {
  return (
    <div className="min-h-screen bg-[#0B0F17] text-white flex flex-col font-sans selection:bg-red selection:text-white">
      <Metadata
        title="Download Buildly Companion — Buildly by RBX"
        description="Download the Buildly desktop companion. Connect your local Studio to ChatGPT and Claude seamlessly over Model Context Protocol."
      />

      {/* Minimal Top Navigation Header */}
      <header className="border-b border-white/10 bg-[#0B0F17]/80 backdrop-blur-md sticky top-0 z-20">
        <div className="max-w-[1200px] mx-auto px-6 h-16 flex items-center justify-between">
          <Link to="/" className="flex items-center gap-3 no-underline">
            <div className="w-8 h-8 rounded-lg bg-gradient-to-br from-red to-red-hover flex items-center justify-center font-bold text-white shadow-lg shadow-red/20">
              B
            </div>
            <div className="flex items-center gap-2">
              <span className="text-base font-bold text-white tracking-tight">Buildly</span>
              <span className="text-[10px] font-bold text-white/60 uppercase tracking-widest bg-white/5 px-2 py-0.5 rounded border border-white/10">
                BY RBX
              </span>
            </div>
          </Link>

          <nav className="flex items-center gap-4 text-sm">
            <Link
              to="/devices"
              className="text-white/70 hover:text-white transition-colors px-3 py-1.5 rounded-lg hover:bg-white/5 font-medium no-underline"
            >
              Dashboard
            </Link>
            <Link
              to="/connectors"
              className="text-white/70 hover:text-white transition-colors px-3 py-1.5 rounded-lg hover:bg-white/5 font-medium no-underline max-sm:hidden"
            >
              Connectors
            </Link>
          </nav>
        </div>
      </header>

      {/* Main Download Experience */}
      <main className="flex-1 max-w-[1200px] w-full mx-auto px-6 py-10 lg:py-16">
        <DownloadHeroSection />
      </main>

      {/* Restrained Hallmark Footer */}
      <footer className="border-t border-white/10 py-8 bg-[#070A0F]">
        <div className="max-w-[1200px] mx-auto px-6 flex items-center justify-between text-xs text-white/50 max-sm:flex-col max-sm:gap-4 max-sm:items-start">
          <div className="flex items-center gap-3">
            <span className="font-semibold text-white/70">Buildly by RBX</span>
            <span>·</span>
            <span>Studio AI Gateway</span>
          </div>
          <div className="flex gap-6">
            <Link to="/privacy" className="hover:text-white transition-colors no-underline">Privacy Policy</Link>
            <Link to="/terms" className="hover:text-white transition-colors no-underline">Terms of Service</Link>
          </div>
        </div>
      </footer>
    </div>
  );
}