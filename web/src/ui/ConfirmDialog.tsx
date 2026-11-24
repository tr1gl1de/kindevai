import { useEffect, useId, useRef } from "react";

interface Props {
  title: string;
  text: string;
  confirmLabel: string;
  cancelLabel: string;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

/** Modal confirmation for destructive actions. Focus starts on Cancel (safe default); Escape cancels. */
export function ConfirmDialog({ title, text, confirmLabel, cancelLabel, busy, onConfirm, onCancel }: Props) {
  const id = useId();
  const cancelRef = useRef<HTMLButtonElement>(null);
  const opener = useRef<Element | null>(null);

  useEffect(() => {
    opener.current = document.activeElement;
    cancelRef.current?.focus();
    return () => (opener.current as HTMLElement | null)?.focus?.();
  }, []);

  return (
    <div className="dialog-backdrop">
      <div
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={`${id}-title`}
        aria-describedby={`${id}-text`}
        className="dialog"
        onKeyDown={(e) => {
          if (e.key === "Escape") onCancel();
        }}
      >
        <h2 id={`${id}-title`}>{title}</h2>
        <p id={`${id}-text`}>{text}</p>
        <div className="dialog-actions">
          <button ref={cancelRef} type="button" className="button" onClick={onCancel}>
            {cancelLabel}
          </button>
          <button type="button" className="button danger" disabled={busy} onClick={onConfirm}>
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
