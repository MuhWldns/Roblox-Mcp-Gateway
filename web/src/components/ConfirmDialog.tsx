import { useEffect, useRef, type KeyboardEvent, type ReactNode } from "react";

export interface ConfirmDialogProps {
  title: string;
  body: ReactNode;
  confirmLabel: string;
  cancelLabel?: string;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

// ConfirmDialog is the shared destructive-action confirmation. It opens with
// focus on the safe choice, keeps Tab focus trapped inside itself, confirms
// with Enter, and cancels with Escape — all reachable from the keyboard.
export default function ConfirmDialog({
  title,
  body,
  confirmLabel,
  cancelLabel = "Cancel",
  busy = false,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const dialogRef = useRef<HTMLDivElement | null>(null);
  const cancelRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    cancelRef.current?.focus();
  }, []);

  function onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    if (event.key === "Escape") {
      event.preventDefault();
      onCancel();
      return;
    }
    if (event.key !== "Tab" || dialogRef.current === null) {
      return;
    }
    const focusables = dialogRef.current.querySelectorAll<HTMLElement>(
      "button:not([disabled]), [href], input:not([disabled]), select, textarea",
    );
    if (focusables.length === 0) {
      return;
    }
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  return (
    <div className="fixed inset-0 bg-navy/50 flex items-center justify-center z-50 p-4 animate-[fadeIn_150ms_ease]">
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        data-testid="confirm-dialog"
        onKeyDown={onKeyDown}
        className="bg-white rounded-lg shadow-lg p-6 max-w-[480px] w-full animate-[dialogEnter_200ms_ease]"
      >
        <h2 className="text-xl font-semibold text-navy mb-3">{title}</h2>
        <div className="text-sm text-text-secondary [&_p]:mb-2 [&_strong]:text-navy">{body}</div>
        <div className="flex gap-3 justify-end mt-5">
          <button
            type="button"
            ref={cancelRef}
            onClick={onCancel}
            className="px-4 py-2 text-sm font-medium border border-border rounded-md text-navy bg-transparent hover:bg-surface-alt transition-colors"
          >
            {cancelLabel}
          </button>
          <button
            type="button"
            onClick={onConfirm}
            disabled={busy}
            className="px-4 py-2 text-sm font-medium bg-red text-white border border-red rounded-md hover:bg-red-hover disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}