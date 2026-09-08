import Metadata from "../components/Metadata";
import DownloadHeroSection from "../routes/DownloadHero";

// Download renders the download landing page that describes the Bridge
// installer and provides the latest version download link.
export default function Download() {
  return (
    <>
      <Metadata
        title="Download Buildly Companion — Buildly by RBX"
        description="Download the Buildly desktop companion. Connect your local Studio to ChatGPT and Claude seamlessly over Model Context Protocol."
      />
      <DownloadHeroSection />
    </>
  );
}