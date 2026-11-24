import { useId, type InputHTMLAttributes, type ReactNode, type Ref, type SelectHTMLAttributes } from "react";

interface Common {
  label: string;
  hint?: string;
  error?: string;
}

function describedBy(hintId: string, errorId: string, hint?: string, error?: string) {
  return [error ? errorId : null, hint ? hintId : null].filter(Boolean).join(" ") || undefined;
}

/** Labelled input with hint and inline error wired to aria-describedby / aria-invalid. */
export function TextField({
  label,
  hint,
  error,
  id,
  ref,
  ...input
}: Common & InputHTMLAttributes<HTMLInputElement> & { ref?: Ref<HTMLInputElement> }) {
  const auto = useId();
  const fid = id ?? auto;
  return (
    <div className="field">
      <label htmlFor={fid}>{label}</label>
      <input
        ref={ref}
        id={fid}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(`${fid}-hint`, `${fid}-error`, hint, error)}
        {...input}
      />
      {error && (
        <p id={`${fid}-error`} className="field-error">
          {error}
        </p>
      )}
      {hint && (
        <p id={`${fid}-hint`} className="field-hint">
          {hint}
        </p>
      )}
    </div>
  );
}

/** Labelled select with the same error/hint wiring as TextField. */
export function SelectField({
  label,
  hint,
  error,
  id,
  ref,
  children,
  ...select
}: Common & SelectHTMLAttributes<HTMLSelectElement> & { children: ReactNode; ref?: Ref<HTMLSelectElement> }) {
  const auto = useId();
  const fid = id ?? auto;
  return (
    <div className="field">
      <label htmlFor={fid}>{label}</label>
      <select
        ref={ref}
        id={fid}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(`${fid}-hint`, `${fid}-error`, hint, error)}
        {...select}
      >
        {children}
      </select>
      {error && (
        <p id={`${fid}-error`} className="field-error">
          {error}
        </p>
      )}
      {hint && (
        <p id={`${fid}-hint`} className="field-hint">
          {hint}
        </p>
      )}
    </div>
  );
}
