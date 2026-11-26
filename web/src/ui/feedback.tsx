import { useEffect, useRef, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Kibi, type KibiMood } from "./Kibi";

/** Page heading that receives focus on mount, so route changes are announced and keyboard focus resets. */
export function PageHeading({ children }: { children: ReactNode }) {
  const ref = useRef<HTMLHeadingElement>(null);
  useEffect(() => ref.current?.focus(), []);
  return (
    <h1 ref={ref} tabIndex={-1} className="page-heading">
      {children}
    </h1>
  );
}

export function Loading({ label }: { label?: string }) {
  const { t } = useTranslation();
  return (
    <div className="loading" aria-busy="true">
      <span className="spinner" aria-hidden="true" />
      <span>{label ?? t("app.loading")}</span>
    </div>
  );
}

export function ErrorState({ message, onRetry }: { message: string; onRetry?: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="error-state">
      <p role="alert">{message}</p>
      {onRetry && (
        <button type="button" className="button" onClick={onRetry}>
          {t("app.retry")}
        </button>
      )}
    </div>
  );
}

/** A benign empty list or first use, with Kibi (KIN-120). Errors use ErrorState, which never shows the mascot. */
export function EmptyState({ title, mood = "idle", children }: { title: string; mood?: KibiMood; children?: ReactNode }) {
  return (
    <section className="empty-state" aria-label={title}>
      <Kibi mood={mood} />
      <h2>{title}</h2>
      {children}
    </section>
  );
}

/** Error summary for a form: announced once, lists how many fields need attention or a form-level message. */
export function FormAlert({ message }: { message?: string }) {
  if (!message) return null;
  return (
    <p role="alert" className="form-alert">
      {message}
    </p>
  );
}
