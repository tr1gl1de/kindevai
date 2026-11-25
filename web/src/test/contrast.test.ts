// @vitest-environment node
// KIN-110, KIN-111: every foreground/background colour pair the stylesheets use meets WCAG 2.2 AA in both themes —
// 4.5:1 for text (1.4.3), 3:1 for borders, focus rings, chart lines and other non-text UI (1.4.11) — and no
// stylesheet bypasses the theme with a hard-coded colour. axe cannot check contrast in jsdom (no layout), so this
// is the real guard. Telegram theme variables are not ours to check; the fallbacks after them are.
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

const srcDir = fileURLToPath(new URL("..", import.meta.url));
const stripComments = (s: string) => s.replace(/\/\*[\s\S]*?\*\//g, "");

const css = stripComments(readFileSync(`${srcDir}/styles.css`, "utf8"));
const featureSheets = readdirSync(`${srcDir}/features`, { withFileTypes: true })
  .filter((d) => d.isDirectory())
  .flatMap((d) =>
    readdirSync(`${srcDir}/features/${d.name}`)
      .filter((f) => f.endsWith(".css"))
      .map((f) => `features/${d.name}/${f}`),
  );
const sheets: Record<string, string> = {
  "styles.css": css,
  ...Object.fromEntries(
    featureSheets.map((f) => [f, stripComments(readFileSync(`${srcDir}/${f}`, "utf8"))]),
  ),
};

// The colour custom properties declared by the `<selector> { … }` block of styles.css, each resolved to the 6-digit
// hex colour it falls back to (null when it has none, which a test rejects). Non-colour tokens (sizes) are skipped.
function tokens(selector: string): Record<string, string | null> {
  const start = css.indexOf(`${selector} {`);
  if (start < 0) throw new Error(`styles.css has no ${selector} block`);
  const body = css.slice(css.indexOf("{", start) + 1, css.indexOf("}", start));
  const out: Record<string, string | null> = {};
  for (const m of body.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) {
    if (!/#|\b(rgba?|hsla?)\(|var\(--tg-/.test(m[2]!)) continue;
    const hex = /#[0-9a-fA-F]{6}\b(?![^)]*#)/.exec(m[2]!.trim());
    out[m[1]!] = hex && /#[0-9a-fA-F]{6}\)?$/.test(m[2]!.trim()) ? hex[0] : null;
  }
  return out;
}

function luminance(hex: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => {
    const c = parseInt(hex.slice(i, i + 2), 16) / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r! + 0.7152 * g! + 0.0722 * b!;
}

function ratio(a: string, b: string): number {
  const [lo, hi] = [luminance(a), luminance(b)].sort((x, y) => x - y);
  return (hi! + 0.05) / (lo! + 0.05);
}

// Light is the default; dark applies when the OS prefers it, unless the Telegram Mini App set data-theme="light",
// and whenever it set data-theme="dark" (KIN-111: the Telegram theme wins over the OS scheme).
const themes = {
  light: tokens(":root"),
  dark: tokens(':root:not([data-theme="light"])'),
};

it("uses the same dark tokens for the OS dark scheme and the Telegram dark theme", () => {
  expect(tokens(':root[data-theme="dark"]')).toEqual(themes.dark);
  expect(Object.keys(themes.dark).sort()).toEqual(Object.keys(themes.light).sort());
});

// Page surfaces content sits on: the page, cards/topbar/code blocks, and warn cards, badges and banners.
const SURFACES = ["--bg", "--surface", "--warn-bg"];
const TEXT = 4.5;
const UI = 3;

// [foreground, backgrounds, minimum ratio] for every pair the stylesheets use.
const PAIRS: [string, string[], number][] = [
  ["--text", SURFACES, TEXT], // body, inputs, buttons, badges, maintenance banner
  ["--muted", SURFACES, TEXT], // hints, counters, log events, chart labels, muted text in warn cards
  ["--link", SURFACES, TEXT], // links, also inside the maintenance banner
  ["--danger", SURFACES, TEXT], // field errors, stderr lines, over-limit counter
  ["--bg", ["--text"], TEXT], // .notice toast
  ["--accent-text", ["--accent"], TEXT], // primary buttons, skip link
  ["--danger-text", ["--danger"], TEXT], // danger buttons, critical badges
  ["--focus", SURFACES, UI], // focus ring: drawn with a 2px offset, so it sits on the surface, never on the fill
  ["--accent", SURFACES, UI], // primary button fill, current nav/revision/segment, chart series
  ["--danger", SURFACES, UI], // invalid inputs, failed-state badge borders, critical threshold
  ["--warn-border", SURFACES, UI], // warn badges/cards, warning threshold
  ["--border-strong", SURFACES, UI], // input, button and segmented control boundaries, chart axes
];

// Colour tokens with no contrast requirement, and why.
const DECORATIVE: Record<string, string> = {
  "--border": "dividers and card/table outlines next to a contrasting fill or text; never a control's only boundary",
};

describe.each(Object.entries(themes))("%s theme", (_, t) => {
  const cases = PAIRS.flatMap(([fg, bgs, min]) => bgs.map((bg) => [fg, bg, min] as const));

  it("resolves every colour token to a 6-digit hex fallback", () => {
    expect(Object.entries(t).filter(([, hex]) => hex === null)).toEqual([]);
  });

  it.each(cases)("%s on %s has at least %s:1", (fg, bg, min) => {
    expect(t[fg]).toMatch(/^#[0-9a-fA-F]{6}$/);
    expect(t[bg]).toMatch(/^#[0-9a-fA-F]{6}$/);
    expect(ratio(t[fg]!, t[bg]!)).toBeGreaterThanOrEqual(min);
  });

  it("checks or exempts every colour token it declares", () => {
    const covered = new Set([...PAIRS.flatMap(([fg, bgs]) => [fg, ...bgs]), ...Object.keys(DECORATIVE)]);
    expect(Object.keys(t).filter((name) => !covered.has(name))).toEqual([]);
  });
});

// Every `selector { declarations }` rule of a sheet, at any nesting depth.
function rules(sheet: string): { selector: string; body: string }[] {
  return [...sheet.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({
    selector: m[1]!.trim().replace(/\s+/g, " "),
    body: m[2]!,
  }));
}

const LITERAL =
  /#[0-9a-fA-F]{3,8}\b|\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch|color|light-dark)\(|\b(white|black|red|green|blue|yellow|orange|purple|pink|teal|silver|gray|grey|navy|maroon|olive|lime|aqua|fuchsia|cyan|magenta|gold|brown)\b/i;
// Literal colours that are not a theme pair: the dialog scrim darkens whatever is behind it in either theme.
const LITERAL_ALLOWED = new Set([".dialog-backdrop"]);

// `property: value` pairs of a rule body; url(…) is dropped first, so data URLs neither split nor match.
function declarations(body: string): [string, string][] {
  return body
    .replace(/url\([^)]*\)/g, "url()")
    .split(";")
    .filter((d) => d.includes(":"))
    .map((d) => [d.slice(0, d.indexOf(":")).trim(), d.slice(d.indexOf(":") + 1).trim()]);
}
const varsIn = (value: string) => [...value.matchAll(/var\((--[\w-]+)/g)].map((m) => m[1]!);

const declared = new Set([...css.matchAll(/(--[\w-]+)\s*:/g)].map((m) => m[1]!));
const paired = (fg: string, bg: string) => PAIRS.some(([f, bgs]) => f === fg && bgs.includes(bg));

describe.each(Object.keys(sheets))("%s", (file) => {
  const sheetRules = rules(sheets[file]!).filter(({ selector }) => !selector.startsWith(":root"));

  it("takes every colour from a theme token", () => {
    const offenders = sheetRules
      .filter(({ selector }) => !LITERAL_ALLOWED.has(selector))
      .flatMap(({ selector, body }) =>
        declarations(body)
          .filter(([, value]) => LITERAL.test(value))
          .map(([prop, value]) => `${selector} { ${prop}: ${value} }`),
      );
    expect(offenders).toEqual([]);
  });

  it("refers only to declared tokens", () => {
    const unknown = sheetRules.flatMap(({ selector, body }) =>
      varsIn(body)
        .filter((name) => !declared.has(name) && !name.startsWith("--tg-theme-"))
        .map((name) => `${selector}: ${name}`),
    );
    expect(unknown).toEqual([]);
  });

  // A rule that sets its own background makes a pair with the text colour and the borders it sets: each such pair
  // must be one PAIRS checks (a border in the fill's own colour, or a decorative one, draws nothing to check).
  it("checks every colour pair a rule sets together", () => {
    const unchecked = sheetRules.flatMap(({ selector, body }) => {
      const decls = declarations(body);
      const bg = decls.filter(([p]) => /^background(-color)?$/.test(p)).flatMap(([, v]) => varsIn(v))[0];
      if (!bg) return [];
      return decls
        .filter(([p]) => p === "color" || /^(border|outline)(-[a-z]+)*$/.test(p))
        .flatMap(([, v]) => varsIn(v))
        .filter((fg) => fg in themes.light && fg !== bg && !(fg in DECORATIVE) && !paired(fg, bg))
        .map((fg) => `${selector}: ${fg} on ${bg}`);
    });
    expect(unchecked).toEqual([]);
  });

  it("draws control boundaries and chart axes with --border-strong, not the decorative --border", () => {
    const control = /(^|[\s,])(input|select|textarea)\b|\.button\b|\.segmented label|^\.history-plot$/;
    const offenders = sheetRules
      .filter(({ selector, body }) => control.test(selector) && /border[\w-]*\s*:[^;]*var\(--border\)/.test(body))
      .map(({ selector }) => selector);
    expect(offenders).toEqual([]);
  });
});
