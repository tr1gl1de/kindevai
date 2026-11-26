import type { ReactNode } from "react";

/** Visual tone of a state (Brand System components.md): neutral, info, success, warning or danger. */
export type Tone = "neutral" | "info" | "success" | "warning" | "danger";

const ICONS: Record<Exclude<Tone, "neutral">, ReactNode> = {
  info: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 11v5M12 8h.01" />
    </>
  ),
  success: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="m8 12 3 3 5-6" />
    </>
  ),
  warning: <path d="M12 4 21 20H3ZM12 10v4M12 17h.01" />,
  danger: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="m9 9 6 6M15 9l-6 6" />
    </>
  ),
};

/**
 * A state label: always text; a non-neutral tone adds a decorative icon and a colour, so the state never rests on
 * colour alone (KIN-120). `className` keeps a feature's own emphasis classes.
 */
export function Badge({ tone = "neutral", className, children }: { tone?: Tone; className?: string; children: ReactNode }) {
  return (
    <span className={`badge tone-${tone}${className ? ` ${className}` : ""}`}>
      {tone !== "neutral" && (
        <svg className="badge-icon" data-tone={tone} aria-hidden="true" focusable="false" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.25" strokeLinecap="round" strokeLinejoin="round">
          {ICONS[tone]}
        </svg>
      )}
      {children}
    </span>
  );
}

/** Severity/fill levels shared by alerts and storage: info, ok, warning/high (attention) and critical. */
export type Level = "info" | "ok" | "warning" | "high" | "critical";
const LEVEL_TONE: Record<Level, Tone> = { info: "info", ok: "success", warning: "warning", high: "warning", critical: "danger" };

/** A level badge; `level-warning`/`level-high` add the warning surface, `level-critical` the danger fill. */
export function LevelBadge({ level, children }: { level: Level; children: ReactNode }) {
  return (
    <Badge tone={LEVEL_TONE[level]} className={`level-badge level-${level}`}>
      {children}
    </Badge>
  );
}
