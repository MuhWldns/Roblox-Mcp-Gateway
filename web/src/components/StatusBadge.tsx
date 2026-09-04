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

export interface StatusBadgeProps {
  status: string;
}

export default function StatusBadge({ status }: StatusBadgeProps) {
  const known = badgeShapes[status];
  return (
    <span data-testid="status-badge" data-status={status}>
      {/* The glyph is decorative; the adjacent text carries the meaning. */}
      <span data-testid="status-shape" aria-hidden="true">
        {known ? known.glyph : "◆"}
      </span>{" "}
      {known ? known.label : status}
    </span>
  );
}
