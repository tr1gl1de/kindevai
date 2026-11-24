import { useEffect, useId, useRef } from "react";
import { createPortal } from "react-dom";

interface Props {
  title: string;
  text: string;
  confirmLabel: string;
  cancelLabel: string;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

/**
 * Modal confirmation for destructive actions. Focus starts on Cancel (safe default), Tab stays inside, Escape
 * cancels, and the rest of the page is inert while it is open. Focus returns to the opener if it still exists.
 */
export function ConfirmDialog({ title, text, confirmLabel, cancelLabel, busy, onConfirm, onCancel }: Props) {
  const id = useId();
  const ref = useRef<HTMLDivElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const host = ref.current?.closest(".dialog-backdrop");
    const others = [...document.body.children].filter((el) => el !== host && !el.hasAttribute("inert"));
    others.forEach((el) => el.setAttribute("inert", ""));
    cancelRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("keydown", onKey);
      others.forEach((el) => el.removeAttribute("inert"));
      if (opener?.isConnected) opener.focus();
    };
  }, [onCancel]);

  function trap(e: React.KeyboardEvent) {
    if (e.key !== "Tab" || !ref.current) return;
    const items = [...ref.current.querySelectorAll<HTMLElement>("button:not([disabled])")];
    const first = items[0];
    const last = items[items.length - 1];
    if (!first || !last) return;
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }

  return createPortal(
    <div className="dialog-backdrop">
      <div
        ref={ref}
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={`${id}-title`}
        aria-describedby={`${id}-text`}
        className="dialog"
        onKeyDown={trap}
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
    </div>,
    document.body,
  );
}
