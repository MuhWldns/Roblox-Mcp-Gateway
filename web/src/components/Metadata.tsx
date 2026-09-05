import { useEffect } from "react";

export interface MetadataProps {
  title: string;
  description: string;
}

// Metadata sets the document head for dynamic route rendering. Called
// from routes that need to set page title and meta description.
export default function Metadata({ title, description }: MetadataProps) {
  useEffect(() => {
    document.title = title;
    const meta = document.querySelector<HTMLMetaElement>(
      'meta[name="description"]',
    );
    if (meta) {
      meta.setAttribute("content", description);
    }
  }, [title, description]);
  return null;
}