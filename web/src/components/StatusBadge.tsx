// StatusBadge renders a status as a text label plus a distinct shape glyph so
// the status is never carried by color alone: screen readers receive the
// word, and color-blind or low-vision visitors still see a distinct shape.

const badgeShapes: Record<string, { glyph: string; label: string }> = {
  online: { glyph: "●", label: "Online" },
  offline: { glyph: "○", label: "Offline" },
  active: { glyph: "✓", label: "Active" },
  ended: { glyph: "—", label: "Ended" },
  revoked: { glyph: "✕", label: "Revoked" },
  expired: { glyph: "■", label: "Expired" },
  ok: { glyph: "✓", label: "OK" },
};

const statusStyles: Record<string, string> = {
  online: "text-success bg-success-bg",
  offline: "text-text-muted bg-surface-alt",
  active: "text-success bg-success-bg",
  ended: "text-text-muted bg-surface-alt",
  revoked: "text-red bg-error-bg",
  expired: "text-warning bg-warning-bg",
  ok: "text-success bg-success-bg",
};

export interface StatusBadgeProps {
  status: string;
}

export default function StatusBadge({ status }: StatusBadgeProps) {
  const known = badgeShapes[status];
  const style = statusStyles[status] ?? "text-text-muted bg-surface-alt";
  return (
    <span
      data-testid="status-badge"
      data-status={status}
      className={`inline-flex items-center gap-2 text-[13px] font-medium px-2 py-0.5 rounded ${style}`}
    >
      {/* The glyph is decorative; the adjacent text carries the meaning. */}
      <span
        data-testid="status-shape"
        aria-hidden="true"
        className={status === "online" ? "animate-pulse" : ""}
      >
        {known ? known.glyph : "◆"}
      </span>{" "}
      {known ? known.label : status}
    </span>
  );
}