import { useCallback, useRef, useState } from "react";

export type FieldErrors<F extends string> = Partial<Record<F, string>>;

/**
 * Field-level errors plus focus management: after a failed submit, focus moves to the first invalid field
 * (in the order fields were registered), as screen-reader and keyboard users expect.
 */
export function useFieldErrors<F extends string>(order: readonly F[]) {
  const [errors, setErrors] = useState<FieldErrors<F>>({});
  const refs = useRef<Partial<Record<F, HTMLElement | null>>>({});

  const register = useCallback(
    (field: F) => (el: HTMLElement | null) => {
      refs.current[field] = el;
    },
    [],
  );

  const show = useCallback(
    (next: FieldErrors<F>) => {
      setErrors(next);
      const first = order.find((f) => next[f]);
      if (first) queueMicrotask(() => refs.current[first]?.focus());
    },
    [order],
  );

  const clear = useCallback((field: F) => {
    setErrors((e) => {
      if (!e[field]) return e;
      const next = { ...e };
      delete next[field];
      return next;
    });
  }, []);

  const count = Object.values(errors).filter(Boolean).length;
  return { errors, show, clear, register, count };
}
