// Source of truth for UI strings. Every key added here must also be added to ru.ts (enforced by types and tests).
export const en = {
  app: {
    title: "AI Platform",
  },
  health: {
    loading: "Connecting to the control plane…",
    online: "Control plane online · {{version}}",
    unreachable: "Control plane unreachable",
  },
  language: {
    label: "Language",
  },
} as const;

type Widen<T> = { [K in keyof T]: T[K] extends string ? string : Widen<T[K]> };
export type Messages = Widen<typeof en>;
