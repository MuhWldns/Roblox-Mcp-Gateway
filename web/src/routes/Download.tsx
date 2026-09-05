import Metadata from "../components/Metadata";
import DownloadHeroSection from "../routes/DownloadHero";

// Download renders the download landing page that describes the Bridge
// installer and provides the latest version download link.
export default function Download() {
  return (
    <>
      <Metadata title="Download RobloxBridge" description="Get a free trial of RobloxBridge — the desktop app that connects your local Roblox Studio to the RobloxKit gateway, so ChatGPT and Claude can control your Studio through MCP." />
      <DownloadHeroSection />
    </>
  );
}